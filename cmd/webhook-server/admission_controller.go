package main

import (
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
type admitFunc func(*v1beta1.AdmissionRequest) ([]patchOperation, error)

// isKubeNamespace checks if the given namespace is a Kubernetes-owned namespace.
func isKubeNamespace(ns string) bool {
	return ns == metav1.NamespacePublic || ns == metav1.NamespaceSystem
}

var errs []string

// doServeAdmitFunc parses the HTTP request for an admission controller webhook, and -- in case of a well-formed
// request -- delegates the admission control logic to the given admitFunc. The response body is then returned as raw
// bytes.
func doServeAdmitFunc(w http.ResponseWriter, r *http.Request, admit admitFunc) ([]byte, error) {
	logrus.Infof("Processing %s request from %s", r.Method, r.RemoteAddr)

	// parse the request body into a json object
	var requestJson map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&requestJson)
	if err != nil {
		errs = append(errs, fmt.Sprintf("could not decode request body: %v", err))
		return nil, err
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, fmt.Errorf("invalid method %s, only POST requests are allowed", r.Method)
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		errs = append(errs, fmt.Sprintf("Could not read request body: %v", err))
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("could not read request body: %v", err)
	}

	if contentType := r.Header.Get("Content-Type"); contentType != jsonContentType {
		errs = append(errs, fmt.Sprintf("unsupported content type %s, only %s is supported", contentType, jsonContentType))
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("unsupported content type %s, only %s is supported", contentType, jsonContentType)
	}

	// Parse the AdmissionReview request.
	var admissionReviewReq v1beta1.AdmissionReview

	if _, _, err := universalDeserializer.Decode(body, nil, &admissionReviewReq); err != nil {
		errs = append(errs, fmt.Sprintf("could not deserialize request: %v", err))
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("could not deserialize request: %v", err)
	} else if admissionReviewReq.Request == nil {
		errs = append(errs, fmt.Sprintf("malformed admission review: request is nil"))
		w.WriteHeader(http.StatusBadRequest)
		return nil, errors.New("malformed admission review: request is nil")
	}

	// Construct the AdmissionReview response.
	admissionReviewResponse := v1beta1.AdmissionReview{
		Response: &v1beta1.AdmissionResponse{
			UID: admissionReviewReq.Request.UID,
		},
	}

	remoteAddress := strings.Split(r.RemoteAddr, ":")
	requesterIP, _ := remoteAddress[0], remoteAddress[1]

	// Convert requestJson["request"].(map[string]interface{})["object"] to unstructured
	objectJson := requestJson["request"].(map[string]interface{})["object"].(map[string]interface{})
	unstructuredObject := &unstructured.Unstructured{Object: objectJson}

	var patchOps []patchOperation
	patchOps, err = admit(admissionReviewReq.Request)
	if err != nil {
		admissionReviewResponse.Response.Allowed = false
		admissionReviewResponse.Response.Result = &metav1.Status{
			Message: err.Error(),
		}
	}

	if unstructuredObject.GetLabels()["app.heimdall.io/owner"] != "" {
		ownerIP := unstructuredObject.GetLabels()["app.heimdall.io/owner"]

		if ownerIP != requesterIP {
			logrus.Warnf("owner ip %s does not match request address %s, denying the request", ownerIP, r.RemoteAddr)
			admissionReviewResponse.Response.Allowed = false
			admissionReviewResponse.Response.Result = &metav1.Status{
				Message: fmt.Sprintf("owner ip %s does not match request address %s, denying the request", ownerIP, r.RemoteAddr),
			}
		} else {
			// Otherwise, encode the patch operations to JSON and return a positive response.
			patchBytes, err := json.Marshal(patchOps)
			if err != nil {
				errs = append(errs, fmt.Sprintf("could not marshal JSON patch: %v", err))
				w.WriteHeader(http.StatusInternalServerError)
				return nil, fmt.Errorf("could not marshal JSON patch: %v", err)
			}
			admissionReviewResponse.Response.Allowed = true
			admissionReviewResponse.Response.Patch = patchBytes

			admissionReviewResponse.Response.Allowed = true
			admissionReviewResponse.Response.Result = &metav1.Status{
				Message: fmt.Sprintf("owner ip %s matches request address %s, admitting the request", ownerIP, r.RemoteAddr),
			}
			logrus.Infof("owner ip %s matches request address %s, admitting the request", ownerIP, r.RemoteAddr)
		}
	}

	// Return the AdmissionReview with a response as JSON.
	bytes, err := json.Marshal(&admissionReviewResponse)
	if err != nil {
		errs = append(errs, fmt.Sprintf("marshaling response: %v", err))
		return nil, fmt.Errorf("marshaling response: %v", err)
	}
	return bytes, nil
}

// serveAdmitFunc is a wrapper around doServeAdmitFunc that adds error handling and logging.
func serveAdmitFunc(w http.ResponseWriter, r *http.Request, admit admitFunc) {
	var writeErr error
	if bytes, err := doServeAdmitFunc(w, r, admit); err != nil {
		for i, err := range errs {
			logrus.Errorf("Error no. %v handling webhook request: %v", i, err)
		}
		logrus.Errorf("Error handling webhook request: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, writeErr = w.Write([]byte(err.Error()))
	} else {
		log.Print("Webhook request handled successfully")
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
