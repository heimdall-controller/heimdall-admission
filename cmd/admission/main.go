package main

import (
	"context"
	"encoding/json"
	"fmt"
	kafka "github.com/Shopify/sarama"
	"github.com/google/uuid"
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
	heimdallTopic    = "heimdall-topic"
)

type ResourceDetails struct {
	MessageID uuid.UUID
	Name      string
	Namespace string
	Kind      string
	Group     string
	Version   string
}

func processResourceChanges(req *v1beta1.AdmissionRequest, senderIP string) ([]patchOperation, error) {
	logrus.Infof("request is valid, validating contents of %s/%s", req.Namespace, req.Name)

	resourceDetails := ResourceDetails{
		MessageID: uuid.New(),
		Name:      req.Name,
		Namespace: req.Namespace,
		Kind:      req.Kind.Kind,
		Group:     req.Kind.Group,
		Version:   req.Kind.Version,
	}

	// Marshal the struct into a JSON string
	resourceDetailsJSON, err := json.Marshal(resourceDetails)
	if err != nil {
		logrus.Errorf("ERROR: admission controller failed JSONifying Resource details: %v", err)
		return nil, fmt.Errorf("ERROR: admision controller failed JSONifying Resource details: %v", err)
	}

	existingObj := &unstructured.Unstructured{}
	newObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.OldObject.Raw, existingObj); err != nil {
		logrus.Errorf("ERROR: admission controller failed decoding existing object: %v", err)
		return nil, fmt.Errorf("ERROR: admision controller failed decoding existing object: %v", err)
	}
	if err := json.Unmarshal(req.Object.Raw, newObj); err != nil {
		logrus.Errorf("ERROR: admission controller failed decoding new object: %v", err)
		return nil, fmt.Errorf("ERROR: admission controller failed decoding new object: %v", err)
	}

	// Check if the objects are equal
	if reflect.DeepEqual(existingObj.Object, newObj.Object) {
		logrus.Infof("ALLOWED: no changes detected, allowing request")
		return nil, nil
	}

	ownerIP := existingObj.GetLabels()[ownerLabel]

	// Check if owner and sender IPs match
	if senderIP == ownerIP {
		logrus.Infof("ALLOWED: owner IP %s matches sender IP %s", ownerIP, senderIP)
		return nil, nil
	}

	// Check if the specs have been changed
	if !reflect.DeepEqual(existingObj.Object["spec"], newObj.Object["spec"]) {
		if err := queueResourceForReconcile(namespace, kafkaClusterName, resourceDetailsJSON); err != nil {
			logrus.Warnf("ERROR: failed to queue resource for reconcile: %v", err)
			return nil, fmt.Errorf("ERROR: failed to queue resource for reconcile: %v", err)
		}
		logrus.Warnf("DENIED: non-owner %s cannot change Spec, resource queued for Reconcile", senderIP)
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
			if err := queueResourceForReconcile(namespace, kafkaClusterName, resourceDetailsJSON); err != nil {
				logrus.Warnf("ERROR: failed to queue resource for reconcile: %v", err)
				return nil, fmt.Errorf("ERROR: failed to queue resource for reconcile: %v", err)
			}
			logrus.Warnf("DENIED: non-owner %s cannot change non-Heimdall label (%s: %s), resource queued for Reconcile", senderIP, k, v)
			return nil, fmt.Errorf("DENIED: non-owner changes are not permitted to non-Heimdall label (%s: %s)", k, v)
		}
	}

	// Permit the request if all checks pass
	logrus.Infof("ALLOWED: request from %s changed a Heimdall label", senderIP)
	return nil, nil
}

func createKafkaTopic(config kafka.Config, brokerList []string) error {
	admin, err := kafka.NewClusterAdmin(brokerList, &config)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close() }()

	// Check if topic already exists
	topicMetadata, err := admin.DescribeTopics([]string{heimdallTopic})
	if err == nil && len(topicMetadata) == 1 {
		// Topic already exists
		return nil
	}

	// Create topic
	topicDetails := kafka.TopicDetail{
		NumPartitions:     2,
		ReplicationFactor: 1,
	}
	err = admin.CreateTopic(heimdallTopic, &topicDetails, false)
	if err != nil {
		return err
	}

	return nil
}

func queueResourceForReconcile(namespace string, kafkaClusterName string, resourceDetails []byte) error {
	// Get Kafka broker list
	brokerList, err := getBrokerList(namespace, kafkaClusterName)
	if err != nil {
		logrus.Errorf("failed to get broker list: %v", err)
		return err
	}

	logrus.Infof("retrieved Kafka broker address %s", brokerList)

	// Set up Kafka producer config
	config := kafka.NewConfig()
	config.Producer.Return.Successes = true
	config.Producer.Return.Errors = true
	config.Producer.RequiredAcks = kafka.NoResponse

	// Connect to Kafka broker
	producer, err := kafka.NewSyncProducer(brokerList, config)
	if err != nil {
		logrus.Errorf("failed to create Kafka producer: %v", err)
		return err
	}
	defer producer.Close()

	err = createKafkaTopic(*config, brokerList)
	if err != nil {
		logrus.Errorf("failed to create Kafka topic: %v", err)
		return err
	}

	// Publish a message to Kafka
	message := &kafka.ProducerMessage{
		Topic: heimdallTopic,
		Value: kafka.StringEncoder(resourceDetails),
	}

	partition, offset, err := producer.SendMessage(message)
	if err != nil {
		logrus.Errorf("failed to send message to Kafka: %v", err)
		return err
	}

	logrus.Infof("sent message to Kafka. Partition: %d, Offset: %d", partition, offset)

	return nil
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
