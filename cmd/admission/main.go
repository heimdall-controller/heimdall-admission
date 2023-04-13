package main

import (
	"encoding/json"
	"errors"
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
		logrus.Warnf("error decoding existing object: %v", err)
		return nil, fmt.Errorf("error decoding existing object: %v", err)
	}
	if err := json.Unmarshal(req.Object.Raw, newObj); err != nil {
		logrus.Warnf("error decoding new object: %v", err)
		return nil, fmt.Errorf("error decoding new object: %v", err)
	}

	// Check if the objects are equal
	if reflect.DeepEqual(existingObj.Object, newObj.Object) {
		logrus.Infof("no changes detected, allowing request to go through")
		return nil, nil
	}

	// Check if owner and sender IPs match
	if senderIP == ownerIP {
		logrus.Infof("owner IP and sender IP match, allowing request to go through")
		return nil, nil
	}

	// Check if the specs have been changed
	if !reflect.DeepEqual(existingObj.Object["spec"], newObj.Object["spec"]) {
		logrus.Errorf("request denied because specs have been changed")
		return nil, errors.New("request denied: specs have been changed")
	}

	// Check if any non-allowed labels have been changed
	allowedLabels := map[string]bool{
		"app.heimdall.io/owner":    true,
		"app.heimdall.io/priority": true,
	}
	existingLabels := existingObj.GetLabels()
	newLabels := newObj.GetLabels()
	for k, v := range newLabels {
		if _, ok := allowedLabels[k]; !ok && existingLabels[k] != v {
			logrus.Errorf("request denied because of invalid label change: %s", k)
			return nil, fmt.Errorf("request denied: changes are not allowed for label %s", k)
		}
	}

	// Permit the request if all checks pass
	logrus.Infof("request allowed")
	return nil, nil
}

// getLabelDifferences returns three sets of label keys:
// 1. Labels that have been added in the new object
// 2. Labels that have been deleted in the new object
// 3. Labels that have changed values in the new object
func getLabelDifferences(existingLabels map[string]string, newLabels map[string]string) (map[string]string, map[string]string, map[string]string) {
	addedLabels := make(map[string]string)
	deletedLabels := make(map[string]string)
	changedLabels := make(map[string]string)

	for k, v := range newLabels {
		if oldValue, ok := existingLabels[k]; !ok {
			addedLabels[k] = v
		} else if oldValue != v {
			changedLabels[k] = v
		}
	}

	for k, v := range existingLabels {
		if _, ok := newLabels[k]; !ok {
			deletedLabels[k] = v
		}
	}

	return addedLabels, deletedLabels, changedLabels
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
