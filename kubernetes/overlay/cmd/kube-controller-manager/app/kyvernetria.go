/*
Copyright 2026 The Kyvernetria Authors.

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

package app

import (
	"context"
	"net/http"
	"time"

	v1 "k8s.io/api/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/record"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"k8s.io/kubernetes/pkg/kyvernetria/controller/gestation"
	"k8s.io/kubernetes/pkg/kyvernetria/controller/placememory"
	"k8s.io/kubernetes/pkg/kyvernetria/controller/relationships"
	"k8s.io/kubernetes/pkg/kyvernetria/controller/worry"
)

// Kyvernetria controller names. The service account of each is the name
// without the "-controller" suffix.
const (
	KyvernetriaGestationController     = "kyvernetria-gestation-controller"
	KyvernetriaPlaceMemoryController   = "kyvernetria-place-memory-controller"
	KyvernetriaRelationshipsController = "kyvernetria-relationships-controller"
	KyvernetriaWorryController         = "kyvernetria-worry-controller"
)

func newKyvernetriaPlaceMemoryControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:        KyvernetriaPlaceMemoryController,
		constructor: newKyvernetriaPlaceMemoryController,
	}
}

func newKyvernetriaPlaceMemoryController(ctx context.Context, controllerContext ControllerContext, controllerName string) (Controller, error) {
	client, err := controllerContext.NewClient("kyvernetria-place-memory")
	if err != nil {
		return nil, err
	}
	f := controllerContext.InformerFactory
	c, err := placememory.New(client, f.Core().V1().Pods(), f.Apps().V1().ReplicaSets(),
		f.Apps().V1().Deployments(), f.Apps().V1().StatefulSets())
	if err != nil {
		return nil, err
	}
	return newControllerLoop(func(ctx context.Context) {
		c.Run(ctx, 2)
	}, controllerName), nil
}

func newKyvernetriaRelationshipsControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:        KyvernetriaRelationshipsController,
		constructor: newKyvernetriaRelationshipsController,
	}
}

func newKyvernetriaRelationshipsController(ctx context.Context, controllerContext ControllerContext, controllerName string) (Controller, error) {
	config, err := controllerContext.NewClientConfig("kyvernetria-relationships")
	if err != nil {
		return nil, err
	}
	crds, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	f := controllerContext.InformerFactory
	c := relationships.New(crds, dyn, f.Core().V1().Services(), f.Apps().V1().Deployments(),
		f.Apps().V1().StatefulSets(), f.Apps().V1().DaemonSets())
	return newControllerLoop(c.Run, controllerName), nil
}

func newKyvernetriaWorryControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:        KyvernetriaWorryController,
		constructor: newKyvernetriaWorryController,
	}
}

func newKyvernetriaWorryController(ctx context.Context, controllerContext ControllerContext, controllerName string) (Controller, error) {
	client, err := controllerContext.NewClient("kyvernetria-worry")
	if err != nil {
		return nil, err
	}
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "kyvernetria-worry"})
	f := controllerContext.InformerFactory
	c := worry.New(client, f.Core().V1().Nodes(), f.Core().V1().Pods(), recorder)
	return newControllerLoop(func(ctx context.Context) {
		broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})
		defer broadcaster.Shutdown()
		c.Run(ctx)
	}, controllerName), nil
}

func newKyvernetriaGestationControllerDescriptor() *ControllerDescriptor {
	return &ControllerDescriptor{
		name:        KyvernetriaGestationController,
		constructor: newKyvernetriaGestationController,
	}
}

func newKyvernetriaGestationController(ctx context.Context, controllerContext ControllerContext, controllerName string) (Controller, error) {
	config, err := controllerContext.NewClientConfig("kyvernetria-gestation")
	if err != nil {
		return nil, err
	}
	client, err := controllerContext.NewClient("kyvernetria-gestation")
	if err != nil {
		return nil, err
	}
	crds, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	meta, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	metrics, err := metricsclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	// Image checks ask registries anonymously; never with credentials.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: gestation.EventSource})
	c := gestation.New(gestation.Clients{
		Kube: client, CRDs: crds, Dynamic: dyn, Metadata: meta, Metrics: metrics,
		HTTPClient: &http.Client{Transport: transport, Timeout: 5 * time.Second},
	}, controllerContext.InformerFactory, recorder)
	return newControllerLoop(func(ctx context.Context) {
		broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})
		defer broadcaster.Shutdown()
		c.Run(ctx)
	}, controllerName), nil
}
