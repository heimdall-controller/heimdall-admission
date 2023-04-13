package main

import (
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"k8s.io/api/admission/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
)

const (
	tlsDir        = `/run/secrets/tls`
	tlsCertFile   = `tls.crt`
	tlsKeyFile    = `tls.key`
	ownerLabel    = `app.heimdall.io/owner`
	priorityLabel = `app.heimdall.io/priority`
)

func processResourceChanges(req *v1beta1.AdmissionRequest, senderIP string, ownerIP string) ([]patchOperation, error) {
	logrus.Infof("triggered admit function for resource %s", req.Name)

	existingObj := &unstructured.Unstructured{}
	newObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.OldObject.Raw, existingObj); err != nil {
		return nil, fmt.Errorf("ERROR: admision controller failed decoding existing object: %v", err)
	}
	if err := json.Unmarshal(req.Object.Raw, newObj); err != nil {
		logrus.Warnf("error decoding new object: %v", err)
		return nil, fmt.Errorf("ERROR: admission controller failed decoding new object: %v", err)
	}

	// Check if the objects are equal
	if reflect.DeepEqual(existingObj.Object, newObj.Object) {
		logrus.Infof("ALLOWED: no changes detected, allowing request")
		return nil, nil
	}

	// Check if owner and sender IPs match
	if senderIP == ownerIP {
		logrus.Infof("ALLOWED: owner IP %s matches sender IP %s", ownerIP, senderIP)
		return nil, nil
	}

	// Check if the specs have been changed
	if !reflect.DeepEqual(existingObj.Object["spec"], newObj.Object["spec"]) {
		return nil, fmt.Errorf("DENIED: non-owner %s cannot change Spec", senderIP)
	}

	// Check if any non-allowed labels have been changed
	allowedLabels := map[string]bool{
		ownerLabel:    true,
		priorityLabel: true,
	}
	existingLabels := existingObj.GetLabels()
	newLabels := newObj.GetLabels()
	for k, v := range newLabels {
		if _, ok := allowedLabels[k]; !ok && existingLabels[k] != v {
			return nil, fmt.Errorf("DENIED: non-owner changes are not permitted non-Heimdall label (%s: %s)", k, v)
		}
	}

	// Permit the request if all checks pass
	logrus.Infof("ALLOWED: request from %s", senderIP)
	return nil, nil
}

func main() {
	certPath := filepath.Join(tlsDir, tlsCertFile)
	keyPath := filepath.Join(tlsDir, tlsKeyFile)

	mux := http.NewServeMux()
	mux.Handle("/mutate", admitFuncHandler(processResourceChanges))
	server := &http.Server{
		// We listen on port 8443 such that we do not need root privileges or extra capabilities for this server.
		// The Service object will take care of mapping this port to the HTTPS port 443.
		Addr:    ":8443",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServeTLS(certPath, keyPath))
}
