package main

import (
	"context"
	"encoding/json"
	"fmt"
	kafka "github.com/Shopify/sarama"
	"github.com/sirupsen/logrus"
	"k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
)

const (
	tlsDir           = `/run/secrets/tls`
	tlsCertFile      = `tls.crt`
	tlsKeyFile       = `tls.key`
	ownerLabel       = `app.heimdall.io/owner`
	priorityLabel    = `app.heimdall.io/priority`
	namespace        = "heimdall"
	kafkaClusterName = "heimdall-kafka-cluster"
)

func processResourceChanges(req *v1beta1.AdmissionRequest, senderIP string, ownerIP string) ([]patchOperation, error) {
	logrus.Infof("triggered admit function for resource %s", req.Name)

	existingObj := &unstructured.Unstructured{}
	newObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.OldObject.Raw, existingObj); err != nil {
		queueResourceForReconcile(namespace, kafkaClusterName)
		return nil, fmt.Errorf("ERROR: admision controller failed decoding existing object: %v", err)
	}
	if err := json.Unmarshal(req.Object.Raw, newObj); err != nil {
		queueResourceForReconcile(namespace, kafkaClusterName)
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
		queueResourceForReconcile(namespace, kafkaClusterName)
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
			queueResourceForReconcile(namespace, kafkaClusterName)
			return nil, fmt.Errorf("DENIED: non-owner changes are not permitted non-Heimdall label (%s: %s)", k, v)
		}
	}

	// Permit the request if all checks pass
	logrus.Infof("ALLOWED: request from %s", senderIP)
	return nil, nil
}

func createKafkaTopic(config kafka.Config, brokerList []string) error {
	admin, err := kafka.NewClusterAdmin(brokerList, &config)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()

	topicName := "heimdall-topic"

	// Check if topic already exists
	topicMetadata, err := admin.DescribeTopics([]string{topicName})
	if err == nil && len(topicMetadata) == 1 {
		// Topic already exists
		return nil
	}

	// Create topic
	topicDetails := kafka.TopicDetail{
		NumPartitions:     2,
		ReplicationFactor: 1,
	}
	err = admin.CreateTopic(topicName, &topicDetails, false)
	if err != nil {
		return err
	}

	return nil
}

func queueResourceForReconcile(namespace string, kafkaClusterName string) {
	// Get Kafka broker list
	brokerList, err := getBrokerList(namespace, kafkaClusterName)
	if err != nil {
		logrus.Errorf("failed to get broker list: %v", err)
		return
	}

	brokerList2 := []string{"10.99.72.78:9092"}

	logrus.Infof("broker list: %v", brokerList)

	// Set up Kafka producer config
	config := kafka.NewConfig()
	config.Producer.Return.Successes = true
	config.Producer.Return.Errors = true

	// Connect to Kafka broker
	producer, err := kafka.NewSyncProducer(brokerList2, config)
	if err != nil {
		logrus.Errorf("failed to create Kafka producer: %v", err)
		return
	}
	defer producer.Close()

	err = createKafkaTopic(*config, brokerList2)
	if err != nil {
		logrus.Errorf("failed to create Kafka topic: %v", err)
		return
	}

	// Publish a message to Kafka
	message := &kafka.ProducerMessage{
		Topic: "heimdall",
		Value: kafka.StringEncoder("namespace/test-name"),
	}

	logrus.Infof("message value: %v", message.Value)

	_, _, err = producer.SendMessage(message)
	if err != nil {
		logrus.Errorf("failed to send message to Kafka: %v", err)
	}
}

func getBrokerList(namespace string, kafkaClusterName string) ([]string, error) {
	// Create Kubernetes clientset
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	// Get list of Kafka broker services
	svcList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("strimzi.io/cluster=%s,strimzi.io/kind=Kafka", kafkaClusterName),
	})
	if err != nil {
		return nil, err
	}

	// Create list of broker addresses in format "broker-address:broker-port"
	brokerList := make([]string, len(svcList.Items))
	for i, svc := range svcList.Items {
		if svc.Spec.ClusterIP != "None" && strings.Contains(svc.Name, "bootstrap") {
			brokerAddress := fmt.Sprintf("%s:%d", svc.Spec.ClusterIP, 9092)
			brokerAddress = strings.Replace(brokerAddress, " ", "", -1)
			brokerList[i] = brokerAddress
		}

	}

	return brokerList, nil
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
