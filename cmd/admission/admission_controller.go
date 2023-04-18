package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sirupsen/logrus"
	"io/ioutil"
	"k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"log"
	"net/http"

	"strings"
)

const (
	jsonContentType = `application/json`
)

var (
	universalDeserializer = serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()
)

// patchOperation is an operation of a JSON patch, see https://tools.ietf.org/html/rfc6902 .
type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// admitFunc is a callback for admission controller logic. Given an AdmissionRequest, it returns the sequence of patch
// operations to be applied in case of success, or the error that will be shown when the operation is rejected.
type admitFunc func(*v1beta1.AdmissionRequest, string) ([]patchOperation, error)

// isKubeNamespace checks if the given namespace is a Kubernetes-owned namespace.
func isKubeNamespace(ns string) bool {
	return ns == metav1.NamespacePublic || ns == metav1.NamespaceSystem
}

// doServeAdmitFunc parses the HTTP request for an admission controller webhook, and -- in case of a well-formed
// request -- delegates the admission control logic to the given admitFunc. The response body is then returned as raw
// bytes.
func doServeAdmitFunc(w http.ResponseWriter, r *http.Request, admit admitFunc) ([]byte, error) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, fmt.Errorf("invalid method %s, only POST requests are allowed", r.Method)
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("could not read request body: %v", err)
	}

	if contentType := r.Header.Get("Content-Type"); contentType != jsonContentType {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("unsupported content type %s, only %s is supported", contentType, jsonContentType)
	}

	// Step 2: Parse the AdmissionReview request.

	var admissionReviewReq v1beta1.AdmissionReview

	if _, _, err := universalDeserializer.Decode(body, nil, &admissionReviewReq); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("could not deserialize request: %v", err)
	} else if admissionReviewReq.Request == nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, errors.New("malformed admission review: request is nil")
	}

	r.Body = ioutil.NopCloser(bytes.NewReader(body))

	// parse the request body into a json object
	var requestJson map[string]interface{}
	err = json.NewDecoder(r.Body).Decode(&requestJson)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		logrus.Errorf("Error decoding request body: %v", err)
		return nil, err
	}

	// Convert requestJson["request"].(map[string]interface{})["object"] to unstructured
	objectJson := requestJson["request"].(map[string]interface{})["object"].(map[string]interface{})
	unstructuredObject := &unstructured.Unstructured{Object: objectJson}

	oldObjectJson := requestJson["request"].(map[string]interface{})["oldObject"].(map[string]interface{})
	oldUnstructuredObject := &unstructured.Unstructured{Object: oldObjectJson}

	ownerIP := ""
	if unstructuredObject.GetLabels()["app.heimdall.io/owner"] != "" {
		ownerIP = unstructuredObject.GetLabels()["app.heimdall.io/owner"]
	} else if oldUnstructuredObject.GetLabels()["app.heimdall.io/owner"] == "" && unstructuredObject.GetLabels()["app.heimdall.io/owner"] == "" {
		w.WriteHeader(http.StatusOK)
		return nil, nil
	} else {
		w.WriteHeader(http.StatusOK)
		return nil, nil
	}

	// if owner = "", then either the object is not a heimdall object OR the label has been removed
	// check for the latter first

	logrus.Infof("────────────────────────────────────────────────────────────")
	logrus.Infof("processing new request for resource %s/%s", unstructuredObject.GetNamespace(), unstructuredObject.GetName())

	senderIP := strings.Split(r.RemoteAddr, ":")[0]
	logrus.Infof("request sender ip: %s", senderIP)

	if ownerIP == "" {
		// allow the request if the owner label is not set
		ownerIP = senderIP
	}

	// Step 3: Construct the AdmissionReview response.

	admissionReviewResponse := v1beta1.AdmissionReview{
		TypeMeta: admissionReviewReq.TypeMeta,
		Response: &v1beta1.AdmissionResponse{
			UID: admissionReviewReq.Request.UID,
		},
	}

	var patchOps []patchOperation
	// Apply the admit() function only for non-Kubernetes namespaces. For objects in Kubernetes namespaces, return
	// an empty set of patch operations.
	if !isKubeNamespace(admissionReviewReq.Request.Namespace) {
		patchOps, err = admit(admissionReviewReq.Request, senderIP)

		if err != nil {
			admissionReviewResponse.Response.Allowed = false
			admissionReviewResponse.Response.Result = &metav1.Status{
				Message: err.Error(),
			}

		} else {
			// Otherwise, encode the patch operations to JSON and return a positive response.
			patchBytes, err := json.Marshal(patchOps)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return nil, fmt.Errorf("could not marshal JSON patch: %v", err)
			}
			admissionReviewResponse.Response.Allowed = true
			admissionReviewResponse.Response.Patch = patchBytes
			admissionReviewResponse.Response.PatchType = new(v1beta1.PatchType)
			*admissionReviewResponse.Response.PatchType = v1beta1.PatchTypeJSONPatch
		}

	}

	// Return the AdmissionReview with a response as JSON.
	bytes, err := json.Marshal(&admissionReviewResponse)
	if err != nil {
		return nil, fmt.Errorf("marshaling response: %v", err)
	}

	return bytes, nil
}

// serveAdmitFunc is a wrapper around doServeAdmitFunc that adds error handling and logging.
func serveAdmitFunc(w http.ResponseWriter, r *http.Request, admit admitFunc) {
	var writeErr error
	if bytes, err := doServeAdmitFunc(w, r, admit); err != nil {
		logrus.Errorf("Error handling webhook request: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, writeErr = w.Write([]byte(err.Error()))
	} else {
		_, writeErr = w.Write(bytes)
	}

	if writeErr != nil {
		log.Printf("Could not write response: %v", writeErr)
	}
}

// admitFuncHandler takes an admitFunc and wraps it into a http.Handler by means of calling serveAdmitFunc.
func admitFuncHandler(admit admitFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveAdmitFunc(w, r, admit)
	})
}
