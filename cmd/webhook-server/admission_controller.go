/*
Copyright (c) 2019 StackRox Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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

// doServeAdmitFunc parses the HTTP request for an admission controller webhook, and -- in case of a well-formed
// request -- delegates the admission control logic to the given admitFunc. The response body is then returned as raw
// bytes.
func doServeAdmitFunc(w http.ResponseWriter, r *http.Request, admit admitFunc) ([]byte, error) {
	// Step 1: Request validation. Only handle POST requests with a body and json content type.

	//TODO process the request
	// 1. get the request body
	// get a copy of the resource in question
	// check if the string "app.heimdall.io" is in the key of any of the labels
	// if it does then extract the ip address appended after the string (which would look like "app.heimdall.io/10.0.0.1")
	// this ip address is the source in which requests are allowed to come from
	// check that the source ip address matches the ip address of the request
	// if it does then allow the request
	// if it does not then deny the request
	// if the string "app.heimdall.io" is not in the key of any of the labels then allow the request

	logrus.Infof("Processing %s request", r.Method)

	// parse the request body into a json object
	var requestJson map[string]interface{}
	err := json.NewDecoder(r.Body).Decode(&requestJson)
	if err != nil {
		return nil, err
	}
	logrus.Info("The request body has been decoded")

	// Convert requestJson["request"].(map[string]interface{})["object"] to unstructured
	objectJson := requestJson["request"].(map[string]interface{})["object"].(map[string]interface{})
	unstructuredObject := &unstructured.Unstructured{Object: objectJson}

	if unstructuredObject.GetLabels()["app.heimdall.io/owner"] != "" {
		owner := unstructuredObject.GetLabels()["app.heimdall.io/owner"]
		logrus.Infof("owner label: %s", owner)
		if owner != r.RemoteAddr {
			logrus.Warnf("owner ip %s does not match request address %s", owner, r.RemoteAddr)
		} else {
			logrus.Infof("owner ip %s matches request address %s", owner, r.RemoteAddr)
		}
	}

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

	// Step 3: Construct the AdmissionReview response.

	admissionReviewResponse := v1beta1.AdmissionReview{
		Response: &v1beta1.AdmissionResponse{
			UID: admissionReviewReq.Request.UID,
		},
	}

	var patchOps []patchOperation
	// Apply the admit() function only for non-Kubernetes namespaces. For objects in Kubernetes namespaces, return
	// an empty set of patch operations.
	if !isKubeNamespace(admissionReviewReq.Request.Namespace) {
		patchOps, err = admit(admissionReviewReq.Request)
	}

	if err != nil {
		// If the handler returned an error, incorporate the error message into the response and deny the object
		// creation.
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

	//// Get the request body from the request
	//requestBody := r.Body
	////TODO Will need to convert this to convert this to unstructured so its easier to use
	////convert requestBody to unstructured
	//
	//// parse the request body into a json object
	//var requestJson map[string]interface{}
	//json.NewDecoder(requestBody).Decode(&requestJson)
	//logrus.Infof("Here is the DECODED request body: %v", requestJson)
	//
	//// Convert requestJson["request"].(map[string]interface{})["object"] to unstructured
	//objectJson := requestJson["request"].(map[string]interface{})["object"].(map[string]interface{})
	//unstructuredObject := &unstructured.Unstructured{Object: objectJson}
	//
	//logrus.Infof("Here is the unstructured object: %v", unstructuredObject)
	//log.Print("Handling webhook request ...")
	//log.Printf("Here is the raw request method: %v", r.Method)
	//log.Printf("Here is the raw request body: %v", r.Body)
	//log.Printf("Here is the raw request response: %v", r.Response)
	//log.Printf("Here is the raw request method: %v", r.Method)
	//// log ip of source of address
	//log.Printf("Here is the raw request remote address: %v", r.RemoteAddr)

	var writeErr error
	if bytes, err := doServeAdmitFunc(w, r, admit); err != nil {
		log.Printf("Error handling webhook request: %v", err)
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
