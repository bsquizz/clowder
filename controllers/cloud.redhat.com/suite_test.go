/*


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

package controllers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	crd "cloud.redhat.com/whippoorwill/v2/apis/cloud.redhat.com/v1alpha1"
	strimzi "cloud.redhat.com/whippoorwill/v2/apis/kafka.strimzi.io/v1beta1"
	// +kubebuilder:scaffold:imports
)

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var logger *zap.Logger

func TestMain(m *testing.M) {
	// call flag.Parse() here if TestMain uses flags
	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseDevMode(true)))
	logger, _ = zap.NewProduction()
	defer logger.Sync()
	logger.Info("bootstrapping test environment")

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}

	cfg, err := testEnv.Start()

	if err != nil {
		logger.Fatal("Error starting test env", zap.Error(err))
	}

	if cfg == nil {
		logger.Fatal("env config was returned nil")
	}

	err = crd.AddToScheme(clientgoscheme.Scheme)

	if err != nil {
		logger.Fatal("Failed to add scheme", zap.Error(err))
	}

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})

	if err != nil {
		logger.Fatal("Failed to create k8s client", zap.Error(err))
	}

	if k8sClient == nil {
		logger.Fatal("k8sClient was returned nil", zap.Error(err))
	}

	stopManager := make(chan struct{})
	go Run(":8080", false, testEnv.Config, stopManager)
	time.Sleep(5000 * time.Millisecond)
	retCode := m.Run()
	logger.Info("Stopping test env...")
	close(stopManager)
	err = testEnv.Stop()

	if err != nil {
		logger.Fatal("Failed to tear down env", zap.Error(err))
	}
	os.Exit(retCode)
}

func TestCreateInsightsApp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger.Info("Creating InsightsApp")

	name := types.NamespacedName{
		Name:      "test",
		Namespace: "default",
	}

	objMeta := metav1.ObjectMeta{
		Name:      name.Name,
		Namespace: name.Namespace,
		Labels: map[string]string{
			"app": "test",
		},
	}

	ibase := crd.InsightsBase{
		ObjectMeta: objMeta,
		Spec: crd.InsightsBaseSpec{
			WebPort:     int32(8080),
			MetricsPort: int32(9000),
			MetricsPath: "/metrics",
		},
	}

	replicas := int32(32)

	iapp := crd.InsightsApp{
		ObjectMeta: objMeta,
		Spec: crd.InsightsAppSpec{
			Image: "test:test",
			Base:  ibase.Name,
			KafkaTopics: []strimzi.KafkaTopicSpec{
				{
					TopicName:  "inventory",
					Partitions: &replicas,
				},
			},
		},
	}

	err := k8sClient.Create(ctx, &ibase)

	if err != nil {
		t.Error(err)
		return
	}

	// Create InsightsApp
	err = k8sClient.Create(ctx, &iapp)

	if err != nil {
		t.Error(err)
		return
	}

	// See if Deployment is created

	d := apps.Deployment{}

	err = fetchWithDefaults(name, &d)

	if err != nil {
		t.Error(err)
		return
	}

	c := d.Spec.Template.Spec.Containers[0]

	if c.Image != iapp.Spec.Image {
		t.Errorf("Bad image spec %s; expected %s", c.Image, iapp.Spec.Image)
	}

	// See if Secret is mounted

	found := false
	for _, mount := range c.VolumeMounts {
		if mount.Name == "config-secret" {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Deployment %s does not have the config volume mounted", d.Name)
		return
	}

	s := core.Service{}

	err = fetchWithDefaults(name, &s)

	if err != nil {
		t.Error(err)
		return
	}

	// Simple test for service right expects there only to be the metrics port
	if len(s.Spec.Ports) > 1 {
		t.Errorf("Bad port count %d; expected 1", len(s.Spec.Ports))
	}

	if s.Spec.Ports[0].Port != ibase.Spec.MetricsPort {
		t.Errorf("Bad port created %d; expected %d", s.Spec.Ports[0].Port, ibase.Spec.MetricsPort)
	}

	topic := strimzi.KafkaTopic{}
	name = types.NamespacedName{
		Namespace: name.Namespace,
		Name:      "inventory",
	}

	err = fetchWithDefaults(name, &topic)

	if err != nil {
		t.Error(err)
		return
	}

	if *topic.Spec.Replicas != replicas {
		t.Errorf("Bad topic replica count %d; expected %d", *topic.Spec.Replicas, replicas)
	}
}

func fetchWithDefaults(name types.NamespacedName, resource runtime.Object) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return fetch(ctx, name, resource, 20, 20*time.Millisecond)
}

func fetch(ctx context.Context, name types.NamespacedName, resource runtime.Object, retryCount int, sleepTime time.Duration) error {
	var err error

	for i := 1; i <= retryCount; i++ {
		err = k8sClient.Get(ctx, name, resource)

		if err == nil {
			return nil
		} else if !k8serr.IsNotFound(err) {
			return err
		}

		time.Sleep(sleepTime)
	}

	return err
}