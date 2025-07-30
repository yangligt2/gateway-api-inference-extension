/*
Copyright 2025 The Kubernetes Authors.

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

package health

import (
	"context"
	"sync/atomic"

	"github.com/go-logr/logr"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

// Checker performs health checks and manages leadership status.
type Checker struct {
	isLeader              atomic.Bool
	datastore             datastore.Datastore
	leaderElectionEnabled bool
	log                   logr.Logger
}

// NewChecker creates a new health checker.
func NewChecker(ds datastore.Datastore, leaderElectionEnabled bool, log logr.Logger) *Checker {
	return &Checker{
		datastore:             ds,
		leaderElectionEnabled: leaderElectionEnabled,
		log:                   log,
	}
}

// Start is a manager.Runnable that blocks until the context is cancelled.
// It is used to update the isLeader field.
func (c *Checker) Start(ctx context.Context) error {
	c.isLeader.Store(true)
	<-ctx.Done()
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (c *Checker) NeedLeaderElection() bool {
	return c.leaderElectionEnabled
}

// Check performs the health check.
func (c *Checker) Check(_ context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	// TODO: we're accepting ANY service name for now as a temporary hack in alignment with
	// upstream issues. See https://github.com/kubernetes-sigs/gateway-api-inference-extension/pull/788
	// if in.Service != extProcPb.ExternalProcessor_ServiceDesc.ServiceName {
	// 	s.logger.V(logutil.DEFAULT).Info("gRPC health check requested unknown service", "available-services", []string{extProcPb.ExternalProcessor_ServiceDesc.ServiceName}, "requested-service", in.Service)
	// 	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVICE_UNKNOWN}, nil
	// }

	if c.leaderElectionEnabled && !c.isLeader.Load() {
		c.log.V(logutil.DEFAULT).Info("gRPC health check not serving (not a leader)", "service", in.Service)
		return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_NOT_SERVING}, nil
	}

	if !c.datastore.PoolHasSynced() {
		c.log.V(logutil.DEFAULT).Info("gRPC health check not serving (pool not synced)", "service", in.Service)
		return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_NOT_SERVING}, nil
	}
	c.log.V(logutil.TRACE).Info("gRPC health check serving", "service", in.Service)
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

var _ manager.Runnable = &Checker{}
var _ manager.LeaderElectionRunnable = &Checker{}
