/*
 *
 * Copyright 2022 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package outlierdetection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/pickfirst/pickfirstleaf"
	"google.golang.org/grpc/balancer/weightedroundrobin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/balancer/stub"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/grpctest"
	iserviceconfig "google.golang.org/grpc/internal/serviceconfig"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/internal/testutils/roundrobin"
	"google.golang.org/grpc/internal/testutils/stats"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/serviceconfig"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/xds/internal/balancer/clusterimpl"

	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

var (
	defaultTestTimeout      = 5 * time.Second
	defaultTestShortTimeout = 10 * time.Millisecond
)

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

// TestParseConfig verifies the ParseConfig() method in the Outlier Detection
// Balancer.
func (s) TestParseConfig(t *testing.T) {
	const errParseConfigName = "errParseConfigBalancer"
	stub.Register(errParseConfigName, stub.BalancerFuncs{
		ParseConfig: func(json.RawMessage) (serviceconfig.LoadBalancingConfig, error) {
			return nil, errors.New("some error")
		},
	})

	parser := bb{}
	const (
		defaultInterval                       = iserviceconfig.Duration(10 * time.Second)
		defaultBaseEjectionTime               = iserviceconfig.Duration(30 * time.Second)
		defaultMaxEjectionTime                = iserviceconfig.Duration(300 * time.Second)
		defaultMaxEjectionPercent             = 10
		defaultSuccessRateStdevFactor         = 1900
		defaultEnforcingSuccessRate           = 100
		defaultSuccessRateMinimumHosts        = 5
		defaultSuccessRateRequestVolume       = 100
		defaultFailurePercentageThreshold     = 85
		defaultEnforcingFailurePercentage     = 0
		defaultFailurePercentageMinimumHosts  = 5
		defaultFailurePercentageRequestVolume = 50
	)
	tests := []struct {
		name    string
		input   string
		wantCfg serviceconfig.LoadBalancingConfig
		wantErr string
	}{
		{
			name: "no-fields-set-should-get-default",
			input: `{
				"childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			wantCfg: &LBConfig{
				Interval:           defaultInterval,
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    defaultMaxEjectionTime,
				MaxEjectionPercent: defaultMaxEjectionPercent,
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},

		{
			name: "some-top-level-fields-set",
			input: `{
				"interval": "15s",
				"maxEjectionTime": "350s",
				"childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			// Should get set fields + defaults for unset fields.
			wantCfg: &LBConfig{
				Interval:           iserviceconfig.Duration(15 * time.Second),
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    iserviceconfig.Duration(350 * time.Second),
				MaxEjectionPercent: defaultMaxEjectionPercent,
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name: "success-rate-ejection-present-but-no-fields",
			input: `{
				"successRateEjection": {},
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			// Should get defaults of success-rate-ejection struct.
			wantCfg: &LBConfig{
				Interval:           defaultInterval,
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    defaultMaxEjectionTime,
				MaxEjectionPercent: defaultMaxEjectionPercent,
				SuccessRateEjection: &SuccessRateEjection{
					StdevFactor:           defaultSuccessRateStdevFactor,
					EnforcementPercentage: defaultEnforcingSuccessRate,
					MinimumHosts:          defaultSuccessRateMinimumHosts,
					RequestVolume:         defaultSuccessRateRequestVolume,
				},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name: "success-rate-ejection-present-partially-set",
			input: `{
				"successRateEjection": {
					"stdevFactor": 1000,
					"minimumHosts": 5
				},
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			// Should get set fields + defaults for others in success rate
			// ejection layer.
			wantCfg: &LBConfig{
				Interval:           defaultInterval,
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    defaultMaxEjectionTime,
				MaxEjectionPercent: defaultMaxEjectionPercent,
				SuccessRateEjection: &SuccessRateEjection{
					StdevFactor:           1000,
					EnforcementPercentage: defaultEnforcingSuccessRate,
					MinimumHosts:          5,
					RequestVolume:         defaultSuccessRateRequestVolume,
				},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name: "success-rate-ejection-present-fully-set",
			input: `{
				"successRateEjection": {
					"stdevFactor": 1000,
					"enforcementPercentage": 50,
					"minimumHosts": 5,
					"requestVolume": 50
				},
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			wantCfg: &LBConfig{
				Interval:           defaultInterval,
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    defaultMaxEjectionTime,
				MaxEjectionPercent: defaultMaxEjectionPercent,
				SuccessRateEjection: &SuccessRateEjection{
					StdevFactor:           1000,
					EnforcementPercentage: 50,
					MinimumHosts:          5,
					RequestVolume:         50,
				},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name: "failure-percentage-ejection-present-but-no-fields",
			input: `{
				"failurePercentageEjection": {},
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			// Should get defaults of failure percentage ejection layer.
			wantCfg: &LBConfig{
				Interval:           defaultInterval,
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    defaultMaxEjectionTime,
				MaxEjectionPercent: defaultMaxEjectionPercent,
				FailurePercentageEjection: &FailurePercentageEjection{
					Threshold:             defaultFailurePercentageThreshold,
					EnforcementPercentage: defaultEnforcingFailurePercentage,
					MinimumHosts:          defaultFailurePercentageMinimumHosts,
					RequestVolume:         defaultFailurePercentageRequestVolume,
				},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name: "failure-percentage-ejection-present-partially-set",
			input: `{
				"failurePercentageEjection": {
					"threshold": 80,
					"minimumHosts": 10
				},
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			// Should get set fields + defaults for others in success rate
			// ejection layer.
			wantCfg: &LBConfig{
				Interval:           defaultInterval,
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    defaultMaxEjectionTime,
				MaxEjectionPercent: defaultMaxEjectionPercent,
				FailurePercentageEjection: &FailurePercentageEjection{
					Threshold:             80,
					EnforcementPercentage: defaultEnforcingFailurePercentage,
					MinimumHosts:          10,
					RequestVolume:         defaultFailurePercentageRequestVolume,
				},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name: "failure-percentage-ejection-present-fully-set",
			input: `{
				"failurePercentageEjection": {
					"threshold": 80,
					"enforcementPercentage": 100,
					"minimumHosts": 10,
					"requestVolume": 40
                },
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			wantCfg: &LBConfig{
				Interval:           defaultInterval,
				BaseEjectionTime:   defaultBaseEjectionTime,
				MaxEjectionTime:    defaultMaxEjectionTime,
				MaxEjectionPercent: defaultMaxEjectionPercent,
				FailurePercentageEjection: &FailurePercentageEjection{
					Threshold:             80,
					EnforcementPercentage: 100,
					MinimumHosts:          10,
					RequestVolume:         40,
				},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{ // to make sure zero values aren't overwritten by defaults
			name: "lb-config-every-field-set-zero-value",
			input: `{
				"interval": "0s",
				"baseEjectionTime": "0s",
				"maxEjectionTime": "0s",
				"maxEjectionPercent": 0,
				"successRateEjection": {
					"stdevFactor": 0,
					"enforcementPercentage": 0,
					"minimumHosts": 0,
					"requestVolume": 0
				},
				"failurePercentageEjection": {
					"threshold": 0,
					"enforcementPercentage": 0,
					"minimumHosts": 0,
					"requestVolume": 0
				},
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			wantCfg: &LBConfig{
				SuccessRateEjection:       &SuccessRateEjection{},
				FailurePercentageEjection: &FailurePercentageEjection{},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name: "lb-config-every-field-set",
			input: `{
				"interval": "10s",
				"baseEjectionTime": "30s",
				"maxEjectionTime": "300s",
				"maxEjectionPercent": 10,
				"successRateEjection": {
					"stdevFactor": 1900,
					"enforcementPercentage": 100,
					"minimumHosts": 5,
					"requestVolume": 100
				},
				"failurePercentageEjection": {
					"threshold": 85,
					"enforcementPercentage": 5,
					"minimumHosts": 5,
					"requestVolume": 50
				},
                "childPolicy": [
				{
					"xds_cluster_impl_experimental": {
						"cluster": "test_cluster"
					}
				}
				]
			}`,
			wantCfg: &LBConfig{
				Interval:           iserviceconfig.Duration(10 * time.Second),
				BaseEjectionTime:   iserviceconfig.Duration(30 * time.Second),
				MaxEjectionTime:    iserviceconfig.Duration(300 * time.Second),
				MaxEjectionPercent: 10,
				SuccessRateEjection: &SuccessRateEjection{
					StdevFactor:           1900,
					EnforcementPercentage: 100,
					MinimumHosts:          5,
					RequestVolume:         100,
				},
				FailurePercentageEjection: &FailurePercentageEjection{
					Threshold:             85,
					EnforcementPercentage: 5,
					MinimumHosts:          5,
					RequestVolume:         50,
				},
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name: "xds_cluster_impl_experimental",
					Config: &clusterimpl.LBConfig{
						Cluster: "test_cluster",
					},
				},
			},
		},
		{
			name:    "interval-is-negative",
			input:   `{"interval": "-10s"}`,
			wantErr: "OutlierDetectionLoadBalancingConfig.interval = -10s; must be >= 0",
		},
		{
			name:    "base-ejection-time-is-negative",
			input:   `{"baseEjectionTime": "-10s"}`,
			wantErr: "OutlierDetectionLoadBalancingConfig.base_ejection_time = -10s; must be >= 0",
		},
		{
			name:    "max-ejection-time-is-negative",
			input:   `{"maxEjectionTime": "-10s"}`,
			wantErr: "OutlierDetectionLoadBalancingConfig.max_ejection_time = -10s; must be >= 0",
		},
		{
			name:    "max-ejection-percent-is-greater-than-100",
			input:   `{"maxEjectionPercent": 150}`,
			wantErr: "OutlierDetectionLoadBalancingConfig.max_ejection_percent = 150; must be <= 100",
		},
		{
			name: "enforcement-percentage-success-rate-is-greater-than-100",
			input: `{
				"successRateEjection": {
					"enforcementPercentage": 150
				}
			}`,
			wantErr: "OutlierDetectionLoadBalancingConfig.SuccessRateEjection.enforcement_percentage = 150; must be <= 100",
		},
		{
			name: "failure-percentage-threshold-is-greater-than-100",
			input: `{
				"failurePercentageEjection": {
					"threshold": 150
				}
			}`,
			wantErr: "OutlierDetectionLoadBalancingConfig.FailurePercentageEjection.threshold = 150; must be <= 100",
		},
		{
			name: "enforcement-percentage-failure-percentage-ejection-is-greater-than-100",
			input: `{
				"failurePercentageEjection": {
					"enforcementPercentage": 150
				}
			}`,
			wantErr: "OutlierDetectionLoadBalancingConfig.FailurePercentageEjection.enforcement_percentage = 150; must be <= 100",
		},
		{
			name: "child-policy-present-but-parse-error",
			input: `{
				"childPolicy": [
				{
					"errParseConfigBalancer": {
						"cluster": "test_cluster"
					}
				}
			]
			}`,
			wantErr: "error parsing loadBalancingConfig for policy \"errParseConfigBalancer\"",
		},
		{
			name: "no-supported-child-policy",
			input: `{
				"childPolicy": [
				{
					"doesNotExistBalancer": {
						"cluster": "test_cluster"
					}
				}
			]
			}`,
			wantErr: "invalid loadBalancingConfig: no supported policies found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotCfg, gotErr := parser.ParseConfig(json.RawMessage(test.input))
			if gotErr != nil && !strings.Contains(gotErr.Error(), test.wantErr) {
				t.Fatalf("ParseConfig(%v) = %v, wantErr %v", test.input, gotErr, test.wantErr)
			}
			if (gotErr != nil) != (test.wantErr != "") {
				t.Fatalf("ParseConfig(%v) = %v, wantErr %v", test.input, gotErr, test.wantErr)
			}
			if test.wantErr != "" {
				return
			}
			if diff := cmp.Diff(gotCfg, test.wantCfg); diff != "" {
				t.Fatalf("parseConfig(%v) got unexpected output, diff (-got +want): %v", string(test.input), diff)
			}
		})
	}
}

func (lbc *LBConfig) Equal(lbc2 *LBConfig) bool {
	if !lbc.EqualIgnoringChildPolicy(lbc2) {
		return false
	}
	return cmp.Equal(lbc.ChildPolicy, lbc2.ChildPolicy)
}

type subConnWithState struct {
	sc    balancer.SubConn
	state balancer.SubConnState
}

func setup(t *testing.T) (*outlierDetectionBalancer, *testutils.BalancerClientConn, func()) {
	t.Helper()
	builder := balancer.Get(Name)
	if builder == nil {
		t.Fatalf("balancer.Get(%q) returned nil", Name)
	}
	tcc := testutils.NewBalancerClientConn(t)
	ch := channelz.RegisterChannel(nil, "test channel")
	t.Cleanup(func() { channelz.RemoveEntry(ch.ID) })
	odB := builder.Build(tcc, balancer.BuildOptions{ChannelzParent: ch})
	return odB.(*outlierDetectionBalancer), tcc, odB.Close
}

type emptyChildConfig struct {
	serviceconfig.LoadBalancingConfig
}

// TestChildBasicOperations tests basic operations of the Outlier Detection
// Balancer and its interaction with its child. The following scenarios are
// tested, in a step by step fashion:
// 1. The Outlier Detection Balancer receives it's first good configuration. The
// balancer is expected to create a child and sent the child it's configuration.
// 2. The Outlier Detection Balancer receives new configuration that specifies a
// child's type, and the new type immediately reports READY inline. The first
// child balancer should be closed and the second child balancer should receive
// a config update.
// 3. The Outlier Detection Balancer is closed. The second child balancer should
// be closed.
func (s) TestChildBasicOperations(t *testing.T) {
	bc := emptyChildConfig{}

	ccsCh := testutils.NewChannel()
	closeCh := testutils.NewChannel()

	stub.Register(t.Name()+"child1", stub.BalancerFuncs{
		UpdateClientConnState: func(_ *stub.BalancerData, ccs balancer.ClientConnState) error {
			ccsCh.Send(ccs.BalancerConfig)
			return nil
		},
		Close: func(*stub.BalancerData) {
			closeCh.Send(nil)
		},
	})

	stub.Register(t.Name()+"child2", stub.BalancerFuncs{
		UpdateClientConnState: func(bd *stub.BalancerData, _ balancer.ClientConnState) error {
			// UpdateState inline to READY to complete graceful switch process
			// synchronously from any UpdateClientConnState call.
			bd.ClientConn.UpdateState(balancer.State{
				ConnectivityState: connectivity.Ready,
				Picker:            &testutils.TestConstPicker{},
			})
			ccsCh.Send(nil)
			return nil
		},
		Close: func(*stub.BalancerData) {
			closeCh.Send(nil)
		},
	})

	od, tcc, _ := setup(t)

	// This first config update should cause a child to be built and forwarded
	// its first update.
	od.UpdateClientConnState(balancer.ClientConnState{
		BalancerConfig: &LBConfig{
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name() + "child1",
				Config: bc,
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	cr, err := ccsCh.Receive(ctx)
	if err != nil {
		t.Fatalf("timed out waiting for UpdateClientConnState on the first child balancer: %v", err)
	}
	if _, ok := cr.(emptyChildConfig); !ok {
		t.Fatalf("Received child policy config of type %T, want %T", cr, emptyChildConfig{})
	}

	// This Update Client Conn State call should cause the first child balancer
	// to close, and a new child to be created and also forwarded its first
	// config update.
	od.UpdateClientConnState(balancer.ClientConnState{
		BalancerConfig: &LBConfig{
			Interval: math.MaxInt64,
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name() + "child2",
				Config: emptyChildConfig{},
			},
		},
	})

	// Verify inline UpdateState() call from the new child eventually makes it's
	// way to the Test Client Conn.
	select {
	case <-ctx.Done():
		t.Fatalf("timeout while waiting for a UpdateState call on the ClientConn")
	case state := <-tcc.NewStateCh:
		if state != connectivity.Ready {
			t.Fatalf("ClientConn received connectivity state %v, want %v", state, connectivity.Ready)
		}
	}

	// Verify the first child balancer closed.
	if _, err = closeCh.Receive(ctx); err != nil {
		t.Fatalf("timed out waiting for the first child balancer to be closed: %v", err)
	}
	// Verify the second child balancer received its first config update.
	if _, err = ccsCh.Receive(ctx); err != nil {
		t.Fatalf("timed out waiting for UpdateClientConnState on the second child balancer: %v", err)
	}
	// Closing the Outlier Detection Balancer should close the newly created
	// child.
	od.Close()
	if _, err = closeCh.Receive(ctx); err != nil {
		t.Fatalf("timed out waiting for the second child balancer to be closed: %v", err)
	}
}

// TestUpdateAddresses tests the functionality of UpdateAddresses and any
// changes in the addresses/plurality of those addresses for a SubConn. The
// Balancer is set up with two upstreams, with one of the upstreams being
// ejected. Initially, there is one SubConn for each address. The following
// scenarios are tested, in a step by step fashion:
// 1. The SubConn not currently ejected switches addresses to the address that
// is ejected. This should cause the SubConn to get ejected.
// 2. Update this same SubConn to multiple addresses. This should cause the
// SubConn to get unejected, as it is no longer being tracked by Outlier
// Detection at that point.
// 3. Update this same SubConn to different addresses, still multiple. This
// should be a noop, as the SubConn is still no longer being tracked by Outlier
// Detection.
// 4. Update this same SubConn to the a single address which is ejected. This
// should cause the SubConn to be ejected.
func (s) TestUpdateAddresses(t *testing.T) {
	scsCh := testutils.NewChannel()
	var scw1, scw2 balancer.SubConn
	var err error
	connectivityCh := make(chan struct{})
	stub.Register(t.Name(), stub.BalancerFuncs{
		UpdateClientConnState: func(bd *stub.BalancerData, _ balancer.ClientConnState) error {
			scw1, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address1"}}, balancer.NewSubConnOptions{
				StateListener: func(balancer.SubConnState) {},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw1.Connect()
			scw2, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address2"}}, balancer.NewSubConnOptions{
				StateListener: func(state balancer.SubConnState) {
					if state.ConnectivityState == connectivity.Ready {
						close(connectivityCh)
					}
				},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw2.Connect()
			bd.ClientConn.UpdateState(balancer.State{
				ConnectivityState: connectivity.Ready,
				Picker: &rrPicker{
					scs: []balancer.SubConn{scw1, scw2},
				},
			})
			return nil
		},
	})

	od, tcc, cleanup := setup(t)
	defer cleanup()

	od.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Endpoints: []resolver.Endpoint{
				{Addresses: []resolver.Address{{Addr: "address1"}}},
				{Addresses: []resolver.Address{{Addr: "address2"}}},
			},
		},
		BalancerConfig: &LBConfig{
			Interval:           iserviceconfig.Duration(10 * time.Second),
			BaseEjectionTime:   iserviceconfig.Duration(30 * time.Second),
			MaxEjectionTime:    iserviceconfig.Duration(300 * time.Second),
			MaxEjectionPercent: 10,
			FailurePercentageEjection: &FailurePercentageEjection{
				Threshold:             50,
				EnforcementPercentage: 100,
				MinimumHosts:          2,
				RequestVolume:         3,
			},
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Transition SubConns to READY so that they can register a health listener.
	for range 2 {
		select {
		case <-ctx.Done():
			t.Fatalf("Timed out waiting for creation of new SubConn.")
		case sc := <-tcc.NewSubConnCh:
			sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
			sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
		}
	}

	// Register health listeners after all the connectivity updates are
	// processed to avoid data races while accessing the health listener within
	// the TestClientConn.
	select {
	case <-ctx.Done():
		t.Fatal("Context timed out waiting for all SubConns to become READY.")
	case <-connectivityCh:
	}

	scw1.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw1, state: healthState})
	})
	scw2.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw2, state: healthState})
	})

	// Setup the system to where one address is ejected and one address
	// isn't.
	select {
	case <-ctx.Done():
		t.Fatal("timeout while waiting for a UpdateState call on the ClientConn")
	case picker := <-tcc.NewPickerCh:
		pi, err := picker.Pick(balancer.PickInfo{})
		if err != nil {
			t.Fatalf("picker.Pick failed with error: %v", err)
		}
		// Simulate 5 successful RPC calls on the first SubConn (the first call
		// to picker.Pick).
		for c := 0; c < 5; c++ {
			pi.Done(balancer.DoneInfo{})
		}
		pi, err = picker.Pick(balancer.PickInfo{})
		if err != nil {
			t.Fatalf("picker.Pick failed with error: %v", err)
		}
		// Simulate 5 failed RPC calls on the second SubConn (the second call to
		// picker.Pick). Thus, when the interval timer algorithm is run, the
		// second SubConn's address should be ejected, which will allow us to
		// further test UpdateAddresses() logic.
		for c := 0; c < 5; c++ {
			pi.Done(balancer.DoneInfo{Err: errors.New("some error")})
		}
		od.intervalTimerAlgorithm()
		// verify StateListener() got called with TRANSIENT_FAILURE for child
		// with address that was ejected.
		gotSCWS, err := scsCh.Receive(ctx)
		if err != nil {
			t.Fatalf("Error waiting for Sub Conn update: %v", err)
		}
		if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
			sc:    scw2,
			state: balancer.SubConnState{ConnectivityState: connectivity.TransientFailure},
		}); err != nil {
			t.Fatalf("Error in Sub Conn update: %v", err)
		}
	}

	// Update scw1 to another address that is currently ejected. This should
	// cause scw1 to get ejected.
	od.UpdateAddresses(scw1, []resolver.Address{{Addr: "address2"}})

	// Verify that update addresses gets forwarded to ClientConn.
	select {
	case <-ctx.Done():
		t.Fatal("timeout while waiting for a UpdateState call on the ClientConn")
	case <-tcc.UpdateAddressesAddrsCh:
	}
	// Verify scw1 got ejected (StateListener called with TRANSIENT_FAILURE).
	gotSCWS, err := scsCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for Sub Conn update: %v", err)
	}
	if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
		sc:    scw1,
		state: balancer.SubConnState{ConnectivityState: connectivity.TransientFailure},
	}); err != nil {
		t.Fatalf("Error in Sub Conn update: %v", err)
	}

	// Update scw1 to multiple addresses. This should cause scw1 to get
	// unejected, as is it no longer being tracked for Outlier Detection.
	od.UpdateAddresses(scw1, []resolver.Address{
		{Addr: "address1"},
		{Addr: "address2"},
	})
	// Verify scw1 got unejected (StateListener called with recent state).
	gotSCWS, err = scsCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for Sub Conn update: %v", err)
	}
	if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
		sc:    scw1,
		state: balancer.SubConnState{ConnectivityState: connectivity.Connecting},
	}); err != nil {
		t.Fatalf("Error in Sub Conn update: %v", err)
	}

	// Update scw1 to a different multiple addresses list. A change of addresses
	// in which the plurality goes from multiple to multiple should be a no-op,
	// as the address continues to be ignored by outlier detection.
	od.UpdateAddresses(scw1, []resolver.Address{
		{Addr: "address2"},
		{Addr: "address3"},
	})
	// Verify no downstream effects.
	sCtx, cancel := context.WithTimeout(context.Background(), defaultTestShortTimeout)
	defer cancel()
	if _, err := scsCh.Receive(sCtx); err == nil {
		t.Fatalf("no SubConn update should have been sent (no SubConn got ejected/unejected)")
	}

	// Update scw1 back to a single address, which is ejected. This should cause
	// the SubConn to be re-ejected.
	od.UpdateAddresses(scw1, []resolver.Address{{Addr: "address2"}})
	// Verify scw1 got ejected (StateListener called with TRANSIENT FAILURE).
	gotSCWS, err = scsCh.Receive(ctx)
	if err != nil {
		t.Fatalf("Error waiting for Sub Conn update: %v", err)
	}
	if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
		sc:    scw1,
		state: balancer.SubConnState{ConnectivityState: connectivity.TransientFailure},
	}); err != nil {
		t.Fatalf("Error in Sub Conn update: %v", err)
	}
}

func scwsEqual(gotSCWS subConnWithState, wantSCWS subConnWithState) error {
	if gotSCWS.sc != wantSCWS.sc || !cmp.Equal(gotSCWS.state, wantSCWS.state, cmp.AllowUnexported(subConnWrapper{}, endpointInfo{}, balancer.SubConnState{}), cmpopts.IgnoreFields(subConnWrapper{}, "scUpdateCh")) {
		return fmt.Errorf("received SubConnState: %+v, want %+v", gotSCWS, wantSCWS)
	}
	return nil
}

type rrPicker struct {
	scs  []balancer.SubConn
	next int
}

func (rrp *rrPicker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	sc := rrp.scs[rrp.next]
	rrp.next = (rrp.next + 1) % len(rrp.scs)
	return balancer.PickResult{SubConn: sc}, nil
}

// TestDurationOfInterval tests the configured interval timer.
// The following scenarios are tested:
// 1. The Outlier Detection Balancer receives it's first config. The balancer
// should configure the timer with whatever is directly specified on the config.
// 2. The Outlier Detection Balancer receives a subsequent config. The balancer
// should configure with whatever interval is configured minus the difference
// between the current time and the previous start timestamp.
// 3. The Outlier Detection Balancer receives a no-op configuration. The
// balancer should not configure a timer at all.
func (s) TestDurationOfInterval(t *testing.T) {
	stub.Register(t.Name(), stub.BalancerFuncs{})

	od, _, cleanup := setup(t)
	defer func(af func(d time.Duration, f func()) *time.Timer) {
		cleanup()
		afterFunc = af
	}(afterFunc)

	durationChan := testutils.NewChannel()
	afterFunc = func(dur time.Duration, _ func()) *time.Timer {
		durationChan.Send(dur)
		return time.NewTimer(math.MaxInt64)
	}

	od.UpdateClientConnState(balancer.ClientConnState{
		BalancerConfig: &LBConfig{
			Interval: iserviceconfig.Duration(8 * time.Second),
			SuccessRateEjection: &SuccessRateEjection{
				StdevFactor:           1900,
				EnforcementPercentage: 100,
				MinimumHosts:          5,
				RequestVolume:         100,
			},
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	d, err := durationChan.Receive(ctx)
	if err != nil {
		t.Fatalf("Error receiving duration from afterFunc() call: %v", err)
	}
	dur := d.(time.Duration)
	// The configured duration should be 8 seconds - what the balancer was
	// configured with.
	if dur != 8*time.Second {
		t.Fatalf("configured duration should have been 8 seconds to start timer")
	}

	// Override time.Now to time.Now() + 5 seconds. This will represent 5
	// seconds already passing for the next check in UpdateClientConnState.
	defer func(n func() time.Time) {
		now = n
	}(now)
	now = func() time.Time {
		return time.Now().Add(time.Second * 5)
	}

	// UpdateClientConnState with an interval of 9 seconds. Due to 5 seconds
	// already passing (from overridden time.Now function), this should start an
	// interval timer of ~4 seconds.
	od.UpdateClientConnState(balancer.ClientConnState{
		BalancerConfig: &LBConfig{
			Interval: iserviceconfig.Duration(9 * time.Second),
			SuccessRateEjection: &SuccessRateEjection{
				StdevFactor:           1900,
				EnforcementPercentage: 100,
				MinimumHosts:          5,
				RequestVolume:         100,
			},
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})

	d, err = durationChan.Receive(ctx)
	if err != nil {
		t.Fatalf("Error receiving duration from afterFunc() call: %v", err)
	}
	dur = d.(time.Duration)
	if dur.Seconds() < 3.5 || 4.5 < dur.Seconds() {
		t.Fatalf("configured duration should have been around 4 seconds to start timer")
	}

	// UpdateClientConnState with a no-op config. This shouldn't configure the
	// interval timer at all due to it being a no-op.
	od.UpdateClientConnState(balancer.ClientConnState{
		BalancerConfig: &LBConfig{
			Interval: iserviceconfig.Duration(10 * time.Second),
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})

	// No timer should have been started.
	sCtx, cancel := context.WithTimeout(context.Background(), defaultTestShortTimeout)
	defer cancel()
	if _, err = durationChan.Receive(sCtx); err == nil {
		t.Fatal("No timer should have started.")
	}
}

// TestEjectUnejectSuccessRate tests the functionality of the interval timer
// algorithm when configured with SuccessRateEjection. The Outlier Detection
// Balancer will be set up with 3 SubConns, each with a different address.
// It tests the following scenarios, in a step by step fashion:
// 1. The three addresses each have 5 successes. The interval timer algorithm should
// not eject any of the addresses.
// 2. Two of the addresses have 5 successes, the third has five failures. The
// interval timer algorithm should eject the third address with five failures.
// 3. The interval timer algorithm is run at a later time past max ejection
// time. The interval timer algorithm should uneject the third address.
func (s) TestEjectUnejectSuccessRate(t *testing.T) {
	scsCh := testutils.NewChannel()
	var scw1, scw2, scw3 balancer.SubConn
	var err error
	connectivityCh := make(chan struct{})
	stub.Register(t.Name(), stub.BalancerFuncs{
		UpdateClientConnState: func(bd *stub.BalancerData, _ balancer.ClientConnState) error {
			scw1, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address1"}}, balancer.NewSubConnOptions{
				StateListener: func(balancer.SubConnState) {},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw1.Connect()
			scw2, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address2"}}, balancer.NewSubConnOptions{
				StateListener: func(balancer.SubConnState) {},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw2.Connect()
			scw3, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address3"}}, balancer.NewSubConnOptions{
				StateListener: func(state balancer.SubConnState) {
					if state.ConnectivityState == connectivity.Ready {
						close(connectivityCh)
					}
				},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw3.Connect()
			bd.ClientConn.UpdateState(balancer.State{
				ConnectivityState: connectivity.Ready,
				Picker: &rrPicker{
					scs: []balancer.SubConn{scw1, scw2, scw3},
				},
			})
			return nil
		},
	})

	od, tcc, cleanup := setup(t)
	defer func() {
		cleanup()
	}()

	od.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Endpoints: []resolver.Endpoint{
				{Addresses: []resolver.Address{{Addr: "address1"}}},
				{Addresses: []resolver.Address{{Addr: "address2"}}},
				{Addresses: []resolver.Address{{Addr: "address3"}}},
			},
		},
		BalancerConfig: &LBConfig{
			Interval:           math.MaxInt64, // so the interval will never run unless called manually in test.
			BaseEjectionTime:   iserviceconfig.Duration(30 * time.Second),
			MaxEjectionTime:    iserviceconfig.Duration(300 * time.Second),
			MaxEjectionPercent: 10,
			FailurePercentageEjection: &FailurePercentageEjection{
				Threshold:             50,
				EnforcementPercentage: 100,
				MinimumHosts:          3,
				RequestVolume:         3,
			},
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Transition the SubConns to READY so that they can register health
	// listeners.
	for range 3 {
		select {
		case <-ctx.Done():
			t.Fatalf("Timed out waiting for creation of new SubConn.")
		case sc := <-tcc.NewSubConnCh:
			sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
			sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
		}
	}

	// Register health listeners after all the connectivity updates are
	// processed to avoid data races while accessing the health listener within
	// the TestClientConn.
	select {
	case <-ctx.Done():
		t.Fatal("Context timed out waiting for all SubConns to become READY.")
	case <-connectivityCh:
	}

	scw1.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw1, state: healthState})
	})
	scw2.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw2, state: healthState})
	})
	scw3.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw3, state: healthState})
	})

	select {
	case <-ctx.Done():
		t.Fatalf("timeout while waiting for a UpdateState call on the ClientConn")
	case picker := <-tcc.NewPickerCh:
		// Set each of the three upstream addresses to have five successes each.
		// This should cause none of the addresses to be ejected as none of them
		// are outliers according to the success rate algorithm.
		for i := 0; i < 3; i++ {
			pi, err := picker.Pick(balancer.PickInfo{})
			if err != nil {
				t.Fatalf("picker.Pick failed with error: %v", err)
			}
			for c := 0; c < 5; c++ {
				pi.Done(balancer.DoneInfo{})
			}
		}

		// Create test metrics recorder
		tmr := stats.NewTestMetricsRecorder()
		od.metricsRecorder = tmr
		od.intervalTimerAlgorithm()

		// verify no StateListener() call on the child, as no addresses got
		// ejected (ejected address will cause an StateListener call).
		sCtx, cancel := context.WithTimeout(context.Background(), defaultTestShortTimeout)
		defer cancel()
		if _, err := scsCh.Receive(sCtx); err == nil {
			t.Fatalf("no SubConn update should have been sent (no SubConn got ejected)")
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_enforced"); got != 0 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_enforced: got %v, want 0", got)
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_unenforced"); got != 0 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_unenforced: got %v, want 0", got)
		}

		// Since no addresses are ejected, a SubConn update should forward down
		// to the child.
		od.scUpdateCh.Put(&scHealthUpdate{
			scw: scw1.(*subConnWrapper),
			state: balancer.SubConnState{
				ConnectivityState: connectivity.Connecting,
			}},
		)

		gotSCWS, err := scsCh.Receive(ctx)
		if err != nil {
			t.Fatalf("Error waiting for Sub Conn update: %v", err)
		}
		if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
			sc:    scw1,
			state: balancer.SubConnState{ConnectivityState: connectivity.Connecting},
		}); err != nil {
			t.Fatalf("Error in Sub Conn update: %v", err)
		}

		// Set two of the upstream addresses to have five successes each, and
		// one of the upstream addresses to have five failures. This should
		// cause the address which has five failures to be ejected according to
		// the SuccessRateAlgorithm.
		for i := 0; i < 2; i++ {
			pi, err := picker.Pick(balancer.PickInfo{})
			if err != nil {
				t.Fatalf("picker.Pick failed with error: %v", err)
			}
			for c := 0; c < 5; c++ {
				pi.Done(balancer.DoneInfo{})
			}
		}
		pi, err := picker.Pick(balancer.PickInfo{})
		if err != nil {
			t.Fatalf("picker.Pick failed with error: %v", err)
		}
		if got, want := pi.SubConn, scw3.(*subConnWrapper).SubConn; got != want {
			t.Fatalf("Unexpected SubConn chosen by picker: got %v, want %v", got, want)
		}
		for c := 0; c < 5; c++ {
			pi.Done(balancer.DoneInfo{Err: errors.New("some error")})
		}

		// should eject address that always errored.
		od.intervalTimerAlgorithm()
		// Due to the address being ejected, the SubConn with that address
		// should be ejected, meaning a TRANSIENT_FAILURE connectivity state
		// gets reported to the child.
		gotSCWS, err = scsCh.Receive(ctx)
		if err != nil {
			t.Fatalf("Error waiting for Sub Conn update: %v", err)
		}
		if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
			sc:    scw3,
			state: balancer.SubConnState{ConnectivityState: connectivity.TransientFailure},
		}); err != nil {
			t.Fatalf("Error in Sub Conn update: %v", err)
		}
		// Only one address should be ejected.
		sCtx, cancel = context.WithTimeout(context.Background(), defaultTestShortTimeout)
		defer cancel()
		if _, err := scsCh.Receive(sCtx); err == nil {
			t.Fatalf("Only one SubConn update should have been sent (only one SubConn got ejected)")
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_enforced"); got != 1 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_enforced: got %v, want 1", got)
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_unenforced"); got != 0 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_unenforced: got %v, want 0", got)
		}

		// Now that an address is ejected, SubConn updates for SubConns using
		// that address should not be forwarded downward. These SubConn updates
		// will be cached to update the child sometime in the future when the
		// address gets unejected.
		od.scUpdateCh.Put(&scHealthUpdate{
			scw:   scw3.(*subConnWrapper),
			state: balancer.SubConnState{ConnectivityState: connectivity.Connecting},
		})
		sCtx, cancel = context.WithTimeout(context.Background(), defaultTestShortTimeout)
		defer cancel()
		if _, err := scsCh.Receive(sCtx); err == nil {
			t.Fatalf("SubConn update should not have been forwarded (the SubConn is ejected)")
		}

		// Override now to cause the interval timer algorithm to always uneject
		// the ejected address. This will always uneject the ejected address
		// because this time is set way past the max ejection time set in the
		// configuration, which will make the next interval timer algorithm run
		// uneject any ejected addresses.
		defer func(n func() time.Time) {
			now = n
		}(now)
		now = func() time.Time {
			return time.Now().Add(time.Second * 1000)
		}
		od.intervalTimerAlgorithm()

		// unejected SubConn should report latest persisted state - which is
		// connecting from earlier.
		gotSCWS, err = scsCh.Receive(ctx)
		if err != nil {
			t.Fatalf("Error waiting for Sub Conn update: %v", err)
		}
		if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
			sc:    scw3,
			state: balancer.SubConnState{ConnectivityState: connectivity.Connecting},
		}); err != nil {
			t.Fatalf("Error in Sub Conn update: %v", err)
		}
	}
}

// TestEjectFailureRate tests the functionality of the interval timer algorithm
// when configured with FailurePercentageEjection, and also the functionality of
// noop configuration. The Outlier Detection Balancer will be set up with 3
// SubConns, each with a different address. It tests the following scenarios, in
// a step by step fashion:
// 1. The three addresses each have 5 successes. The interval timer algorithm
// should not eject any of the addresses.
// 2. Two of the addresses have 5 successes, the third has five failures. The
// interval timer algorithm should eject the third address with five failures.
// 3. The Outlier Detection Balancer receives a subsequent noop config update.
// The balancer should uneject all ejected addresses.
func (s) TestEjectFailureRate(t *testing.T) {
	scsCh := testutils.NewChannel()
	var scw1, scw2, scw3 balancer.SubConn
	var err error
	connectivityCh := make(chan struct{})
	stub.Register(t.Name(), stub.BalancerFuncs{
		UpdateClientConnState: func(bd *stub.BalancerData, _ balancer.ClientConnState) error {
			if scw1 != nil { // UpdateClientConnState was already called, no need to recreate SubConns.
				return nil
			}
			scw1, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address1"}}, balancer.NewSubConnOptions{
				StateListener: func(balancer.SubConnState) {},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw1.Connect()
			scw2, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address2"}}, balancer.NewSubConnOptions{
				StateListener: func(balancer.SubConnState) {},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw2.Connect()
			scw3, err = bd.ClientConn.NewSubConn([]resolver.Address{{Addr: "address3"}}, balancer.NewSubConnOptions{
				StateListener: func(scs balancer.SubConnState) {
					if scs.ConnectivityState == connectivity.Ready {
						close(connectivityCh)
					}
				},
			})
			if err != nil {
				t.Errorf("error in od.NewSubConn call: %v", err)
			}
			scw3.Connect()
			return nil
		},
	})

	od, tcc, cleanup := setup(t)
	defer func() {
		cleanup()
	}()

	od.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Endpoints: []resolver.Endpoint{
				{Addresses: []resolver.Address{{Addr: "address1"}}},
				{Addresses: []resolver.Address{{Addr: "address2"}}},
				{Addresses: []resolver.Address{{Addr: "address3"}}},
			},
		},
		BalancerConfig: &LBConfig{
			Interval:           math.MaxInt64, // so the interval will never run unless called manually in test.
			BaseEjectionTime:   iserviceconfig.Duration(30 * time.Second),
			MaxEjectionTime:    iserviceconfig.Duration(300 * time.Second),
			MaxEjectionPercent: 10,
			SuccessRateEjection: &SuccessRateEjection{
				StdevFactor:           500,
				EnforcementPercentage: 100,
				MinimumHosts:          3,
				RequestVolume:         3,
			},
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})

	od.UpdateState(balancer.State{
		ConnectivityState: connectivity.Ready,
		Picker: &rrPicker{
			scs: []balancer.SubConn{scw1, scw2, scw3},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Transition the SubConns to READY so that they can register health
	// listeners.
	for range 3 {
		select {
		case <-ctx.Done():
			t.Fatal("Timed out waiting for creation of new SubConn.")
		case sc := <-tcc.NewSubConnCh:
			sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Connecting})
			sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
		}
	}
	// Register health listeners after all the connectivity updates are
	// processed to avoid data races while accessing the health listener within
	// the TestClientConn.
	select {
	case <-ctx.Done():
		t.Fatal("Context timed out waiting for all SubConns to become READY.")
	case <-connectivityCh:
	}

	scw1.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw1, state: healthState})
	})
	scw2.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw2, state: healthState})
	})
	scw3.RegisterHealthListener(func(healthState balancer.SubConnState) {
		scsCh.Send(subConnWithState{sc: scw3, state: healthState})
	})

	select {
	case <-ctx.Done():
		t.Fatalf("timeout while waiting for a UpdateState call on the ClientConn")
	case picker := <-tcc.NewPickerCh:
		// Set each upstream address to have five successes each. This should
		// cause none of the addresses to be ejected as none of them are below
		// the failure percentage threshold.
		for i := 0; i < 3; i++ {
			pi, err := picker.Pick(balancer.PickInfo{})
			if err != nil {
				t.Fatalf("picker.Pick failed with error: %v", err)
			}
			for c := 0; c < 5; c++ {
				pi.Done(balancer.DoneInfo{})
			}
		}
		tmr := stats.NewTestMetricsRecorder()
		od.metricsRecorder = tmr

		od.intervalTimerAlgorithm()
		sCtx, cancel := context.WithTimeout(context.Background(), defaultTestShortTimeout)
		defer cancel()
		if _, err := scsCh.Receive(sCtx); err == nil {
			t.Fatalf("no SubConn update should have been sent (no SubConn got ejected)")
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_enforced"); got != 0 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_enforced: got %v, want 0", got)
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_unenforced"); got != 0 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_unenforced: got %v, want 0", got)
		}

		// Set two upstream addresses to have five successes each, and one
		// upstream address to have five failures. This should cause the address
		// with five failures to be ejected according to the Failure Percentage
		// Algorithm.
		for i := 0; i < 2; i++ {
			pi, err := picker.Pick(balancer.PickInfo{})
			if err != nil {
				t.Fatalf("picker.Pick failed with error: %v", err)
			}
			for c := 0; c < 5; c++ {
				pi.Done(balancer.DoneInfo{})
			}
		}
		pi, err := picker.Pick(balancer.PickInfo{})
		if err != nil {
			t.Fatalf("picker.Pick failed with error: %v", err)
		}
		for c := 0; c < 5; c++ {
			pi.Done(balancer.DoneInfo{Err: errors.New("some error")})
		}

		// should eject address that always errored.
		od.intervalTimerAlgorithm()

		// verify StateListener() got called with TRANSIENT_FAILURE for child
		// in address that was ejected.
		gotSCWS, err := scsCh.Receive(ctx)
		if err != nil {
			t.Fatalf("Error waiting for Sub Conn update: %v", err)
		}
		if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
			sc:    scw3,
			state: balancer.SubConnState{ConnectivityState: connectivity.TransientFailure},
		}); err != nil {
			t.Fatalf("Error in Sub Conn update: %v", err)
		}

		// verify only one address got ejected.
		sCtx, cancel = context.WithTimeout(context.Background(), defaultTestShortTimeout)
		defer cancel()
		if _, err := scsCh.Receive(sCtx); err == nil {
			t.Fatalf("Only one SubConn update should have been sent (only one SubConn got ejected)")
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_enforced"); got != 1 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_enforced: got %v, want 1", got)
		}
		if got, _ := tmr.Metric("grpc.lb.outlier_detection.ejections_unenforced"); got != 0 {
			t.Errorf("Metric grpc.lb.outlier_detection.ejections_unenforced: got %v, want 0", got)
		}

		// upon the Outlier Detection balancer being reconfigured with a noop
		// configuration, every ejected SubConn should be unejected.
		od.UpdateClientConnState(balancer.ClientConnState{
			ResolverState: resolver.State{
				Endpoints: []resolver.Endpoint{
					{Addresses: []resolver.Address{{Addr: "address1"}}},
					{Addresses: []resolver.Address{{Addr: "address2"}}},
					{Addresses: []resolver.Address{{Addr: "address3"}}},
				},
			},
			BalancerConfig: &LBConfig{
				Interval:           math.MaxInt64,
				BaseEjectionTime:   iserviceconfig.Duration(30 * time.Second),
				MaxEjectionTime:    iserviceconfig.Duration(300 * time.Second),
				MaxEjectionPercent: 10,
				ChildPolicy: &iserviceconfig.BalancerConfig{
					Name:   t.Name(),
					Config: emptyChildConfig{},
				},
			},
		})
		gotSCWS, err = scsCh.Receive(ctx)
		if err != nil {
			t.Fatalf("Error waiting for Sub Conn update: %v", err)
		}
		if err = scwsEqual(gotSCWS.(subConnWithState), subConnWithState{
			sc:    scw3,
			state: balancer.SubConnState{ConnectivityState: connectivity.Connecting},
		}); err != nil {
			t.Fatalf("Error in Sub Conn update: %v", err)
		}
	}
}

// TestConcurrentOperations calls different operations on the balancer in
// separate goroutines to test for any race conditions and deadlocks. It also
// uses a child balancer which verifies that no operations on the child get
// called after the child balancer is closed.
func (s) TestConcurrentOperations(t *testing.T) {
	closed := grpcsync.NewEvent()
	stub.Register(t.Name(), stub.BalancerFuncs{
		UpdateClientConnState: func(*stub.BalancerData, balancer.ClientConnState) error {
			if closed.HasFired() {
				t.Error("UpdateClientConnState was called after Close(), which breaks the balancer API")
			}
			return nil
		},
		ResolverError: func(*stub.BalancerData, error) {
			if closed.HasFired() {
				t.Error("ResolverError was called after Close(), which breaks the balancer API")
			}
		},
		Close: func(*stub.BalancerData) {
			closed.Fire()
		},
		ExitIdle: func(*stub.BalancerData) {
			if closed.HasFired() {
				t.Error("ExitIdle was called after Close(), which breaks the balancer API")
			}
		},
	})

	od, tcc, cleanup := setup(t)
	defer func() {
		cleanup()
	}()

	od.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Endpoints: []resolver.Endpoint{
				{Addresses: []resolver.Address{{Addr: "address1"}}},
				{Addresses: []resolver.Address{{Addr: "address2"}}},
				{Addresses: []resolver.Address{{Addr: "address3"}}},
			},
		},
		BalancerConfig: &LBConfig{
			Interval:           math.MaxInt64, // so the interval will never run unless called manually in test.
			BaseEjectionTime:   iserviceconfig.Duration(30 * time.Second),
			MaxEjectionTime:    iserviceconfig.Duration(300 * time.Second),
			MaxEjectionPercent: 10,
			SuccessRateEjection: &SuccessRateEjection{ // Have both Success Rate and Failure Percentage to step through all the interval timer code
				StdevFactor:           500,
				EnforcementPercentage: 100,
				MinimumHosts:          3,
				RequestVolume:         3,
			},
			FailurePercentageEjection: &FailurePercentageEjection{
				Threshold:             50,
				EnforcementPercentage: 100,
				MinimumHosts:          3,
				RequestVolume:         3,
			},
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	scw1, err := od.NewSubConn([]resolver.Address{{Addr: "address1"}}, balancer.NewSubConnOptions{})
	if err != nil {
		t.Fatalf("error in od.NewSubConn call: %v", err)
	}
	if err != nil {
		t.Fatalf("error in od.NewSubConn call: %v", err)
	}

	scw2, err := od.NewSubConn([]resolver.Address{{Addr: "address2"}}, balancer.NewSubConnOptions{})
	if err != nil {
		t.Fatalf("error in od.NewSubConn call: %v", err)
	}

	scw3, err := od.NewSubConn([]resolver.Address{{Addr: "address3"}}, balancer.NewSubConnOptions{})
	if err != nil {
		t.Fatalf("error in od.NewSubConn call: %v", err)
	}

	od.UpdateState(balancer.State{
		ConnectivityState: connectivity.Ready,
		Picker: &rrPicker{
			scs: []balancer.SubConn{scw2, scw3},
		},
	})

	var picker balancer.Picker
	select {
	case <-ctx.Done():
		t.Fatalf("timeout while waiting for a UpdateState call on the ClientConn")
	case picker = <-tcc.NewPickerCh:
	}

	finished := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-finished:
				return
			default:
			}
			pi, err := picker.Pick(balancer.PickInfo{})
			if err != nil {
				continue
			}
			pi.Done(balancer.DoneInfo{})
			pi.Done(balancer.DoneInfo{Err: errors.New("some error")})
			time.Sleep(1 * time.Nanosecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-finished:
				return
			default:
			}
			od.intervalTimerAlgorithm()
		}
	}()

	// call Outlier Detection's balancer.ClientConn operations asynchronously.
	// balancer.ClientConn operations have no guarantee from the API to be
	// called synchronously.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-finished:
				return
			default:
			}
			od.UpdateState(balancer.State{
				ConnectivityState: connectivity.Ready,
				Picker: &rrPicker{
					scs: []balancer.SubConn{scw2, scw3},
				},
			})
			time.Sleep(1 * time.Nanosecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		od.NewSubConn([]resolver.Address{{Addr: "address4"}}, balancer.NewSubConnOptions{})
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		scw1.Shutdown()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		od.UpdateAddresses(scw2, []resolver.Address{{Addr: "address3"}})
	}()

	// Call balancer.Balancers synchronously in this goroutine, upholding the
	// balancer.Balancer API guarantee of synchronous calls.
	od.UpdateClientConnState(balancer.ClientConnState{ // This will delete addresses and flip to no op
		ResolverState: resolver.State{
			Endpoints: []resolver.Endpoint{{Addresses: []resolver.Address{{Addr: "address1"}}}},
		},
		BalancerConfig: &LBConfig{
			Interval: math.MaxInt64,
			ChildPolicy: &iserviceconfig.BalancerConfig{
				Name:   t.Name(),
				Config: emptyChildConfig{},
			},
		},
	})

	// Call balancer.Balancers synchronously in this goroutine, upholding the
	// balancer.Balancer API guarantee.
	od.updateSubConnState(scw1.(*subConnWrapper), balancer.SubConnState{
		ConnectivityState: connectivity.Connecting,
	})
	od.ResolverError(errors.New("some error"))
	od.ExitIdle()
	od.Close()
	close(finished)
	wg.Wait()
}

// Test verifies that outlier detection doesn't eject subchannels created by
// the new pickfirst balancer when pickfirst is a non-leaf policy, i.e. not
// under a petiole policy. When pickfirst is not under a petiole policy, it will
// not register a health listener. pickfirst will still set the address
// attribute to disable ejection through the raw connectivity listener. When
// Outlier Detection processes a health update and sees the health listener is
// enabled but a health listener is not registered, it will drop the ejection
// update.
func (s) TestPickFirstHealthListenerDisabled(t *testing.T) {
	backend := &stubserver.StubServer{
		EmptyCallF: func(context.Context, *testpb.Empty) (*testpb.Empty, error) {
			return nil, errors.New("some error")
		},
	}
	if err := backend.StartServer(); err != nil {
		t.Fatalf("Failed to start backend: %v", err)
	}
	defer backend.Stop()
	t.Logf("Started bad TestService backend at: %q", backend.Address)

	// The interval is intentionally kept very large, the interval algorithm
	// will be triggered manually.
	odCfg := &LBConfig{
		Interval:         iserviceconfig.Duration(300 * time.Second),
		BaseEjectionTime: iserviceconfig.Duration(300 * time.Second),
		MaxEjectionTime:  iserviceconfig.Duration(500 * time.Second),
		FailurePercentageEjection: &FailurePercentageEjection{
			Threshold:             50,
			EnforcementPercentage: 100,
			MinimumHosts:          0,
			RequestVolume:         2,
		},
		MaxEjectionPercent: 100,
		ChildPolicy: &iserviceconfig.BalancerConfig{
			Name: pickfirstleaf.Name,
		},
	}

	lbChan := make(chan *outlierDetectionBalancer, 1)
	bf := stub.BalancerFuncs{
		Init: func(bd *stub.BalancerData) {
			bd.ChildBalancer = balancer.Get(Name).Build(bd.ClientConn, bd.BuildOptions)
			lbChan <- bd.ChildBalancer.(*outlierDetectionBalancer)
		},
		Close: func(bd *stub.BalancerData) {
			bd.ChildBalancer.Close()
		},
		UpdateClientConnState: func(bd *stub.BalancerData, ccs balancer.ClientConnState) error {
			ccs.BalancerConfig = odCfg
			return bd.ChildBalancer.UpdateClientConnState(ccs)
		},
	}

	stub.Register(t.Name(), bf)

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(fmt.Sprintf(`{ "loadBalancingConfig": [{%q: {}}] }`, t.Name())),
	}
	cc, err := grpc.NewClient(backend.Address, opts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() failed: %v", err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	testServiceClient := testgrpc.NewTestServiceClient(cc)
	testServiceClient.EmptyCall(ctx, &testpb.Empty{})
	testutils.AwaitState(ctx, t, cc, connectivity.Ready)

	// Failing request should not cause ejection.
	testServiceClient.EmptyCall(ctx, &testpb.Empty{})
	testServiceClient.EmptyCall(ctx, &testpb.Empty{})
	testServiceClient.EmptyCall(ctx, &testpb.Empty{})
	testServiceClient.EmptyCall(ctx, &testpb.Empty{})

	// Run the interval algorithm.
	select {
	case <-ctx.Done():
		t.Fatal("Timed out waiting for the outlier detection LB policy to be built.")
	case od := <-lbChan:
		od.intervalTimerAlgorithm()
	}

	shortCtx, shortCancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer shortCancel()
	testutils.AwaitNoStateChange(shortCtx, t, cc, connectivity.Ready)
}

// Tests handling of endpoints with multiple addresses. The test creates two
// endpoints, each with two addresses. The first endpoint has a backend that
// always returns errors. The test verifies that the first endpoint is ejected
// after running the intervalTimerAlgorithm. The test stops the unhealthy
// backend and verifies that the second backend in the first endpoint is dialed
// but it doesn't receive requests due to its ejection status. The test stops
// the connected backend in the second endpoint and verifies that requests
// start going to the second address in the second endpoint. The test reduces
// the ejection interval and runs the intervalTimerAlgorithm again. The test
// verifies that the first endpoint is unejected and requests reach both
// endpoints.
func (s) TestMultipleAddressesPerEndpoint(t *testing.T) {
	unhealthyBackend := &stubserver.StubServer{
		EmptyCallF: func(context.Context, *testpb.Empty) (*testpb.Empty, error) {
			return nil, errors.New("some error")
		},
	}
	if err := unhealthyBackend.StartServer(); err != nil {
		t.Fatalf("Failed to start backend: %v", err)
	}
	defer unhealthyBackend.Stop()
	t.Logf("Started unhealthy TestService backend at: %q", unhealthyBackend.Address)

	healthyBackends := make([]*stubserver.StubServer, 3)
	for i := 0; i < 3; i++ {
		healthyBackends[i] = stubserver.StartTestService(t, nil)
		defer healthyBackends[i].Stop()
	}

	wrrCfg, err := balancer.Get(weightedroundrobin.Name).(balancer.ConfigParser).ParseConfig(json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("Failed to parse %q config: %v", weightedroundrobin.Name, err)
	}
	// The interval is intentionally kept very large, the interval algorithm
	// will be triggered manually.
	odCfg := &LBConfig{
		Interval:         iserviceconfig.Duration(300 * time.Second),
		BaseEjectionTime: iserviceconfig.Duration(300 * time.Second),
		MaxEjectionTime:  iserviceconfig.Duration(300 * time.Second),
		FailurePercentageEjection: &FailurePercentageEjection{
			Threshold:             50,
			EnforcementPercentage: 100,
			MinimumHosts:          0,
			RequestVolume:         2,
		},
		MaxEjectionPercent: 100,
		ChildPolicy: &iserviceconfig.BalancerConfig{
			Name:   weightedroundrobin.Name,
			Config: wrrCfg,
		},
	}

	lbChan := make(chan *outlierDetectionBalancer, 1)
	bf := stub.BalancerFuncs{
		Init: func(bd *stub.BalancerData) {
			bd.ChildBalancer = balancer.Get(Name).Build(bd.ClientConn, bd.BuildOptions)
			lbChan <- bd.ChildBalancer.(*outlierDetectionBalancer)
		},
		Close: func(bd *stub.BalancerData) {
			bd.ChildBalancer.Close()
		},
		UpdateClientConnState: func(bd *stub.BalancerData, ccs balancer.ClientConnState) error {
			ccs.BalancerConfig = odCfg
			return bd.ChildBalancer.UpdateClientConnState(ccs)
		},
	}

	stub.Register(t.Name(), bf)
	r := manual.NewBuilderWithScheme("whatever")
	endpoints := []resolver.Endpoint{
		{
			Addresses: []resolver.Address{
				{Addr: unhealthyBackend.Address},
				{Addr: healthyBackends[0].Address},
			},
		},
		{
			Addresses: []resolver.Address{
				{Addr: healthyBackends[1].Address},
				{Addr: healthyBackends[2].Address},
			},
		},
	}

	r.InitialState(resolver.State{
		Endpoints: endpoints,
	})
	dialer := testutils.NewBlockingDialer()
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(fmt.Sprintf(`{ "loadBalancingConfig": [{%q: {}}] }`, t.Name())),
		grpc.WithResolvers(r),
		grpc.WithContextDialer(dialer.DialContext),
	}
	cc, err := grpc.NewClient(r.Scheme()+":///", opts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() failed: %v", err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	client := testgrpc.NewTestServiceClient(cc)
	client.EmptyCall(ctx, &testpb.Empty{})
	testutils.AwaitState(ctx, t, cc, connectivity.Ready)

	// Wait until both endpoints start receiving requests.
	addrsSeen := map[string]bool{}
	for ; ctx.Err() == nil && len(addrsSeen) < 2; <-time.After(time.Millisecond) {
		var peer peer.Peer
		client.EmptyCall(ctx, &testpb.Empty{}, grpc.Peer(&peer))
		addrsSeen[peer.String()] = true
	}

	if len(addrsSeen) < 2 {
		t.Fatalf("Context timed out waiting for requests to reach both endpoints.")
	}

	// Make 2 requests to each endpoint and verify the first endpoint gets
	// ejected.
	for i := 0; i < 2*len(endpoints); i++ {
		client.EmptyCall(ctx, &testpb.Empty{})
	}
	var od *outlierDetectionBalancer
	select {
	case <-ctx.Done():
		t.Fatal("Timed out waiting for the outlier detection LB policy to be built.")
	case od = <-lbChan:
	}
	od.intervalTimerAlgorithm()

	// The first endpoint should be ejected, requests should only go to
	// endpoints[1].
	if err := roundrobin.CheckRoundRobinRPCs(ctx, client, []resolver.Address{endpoints[1].Addresses[0]}); err != nil {
		t.Fatalf("RPCs didn't go to the second endpoint: %v", err)
	}

	// Shutdown the unhealthy backend. The second address in the endpoint should
	// be connected, but it should be ejected by outlier detection.
	hold := dialer.Hold(healthyBackends[0].Address)
	unhealthyBackend.Stop()
	if hold.Wait(ctx) != true {
		t.Fatalf("Timeout waiting for second address in endpoint[0] with address %q to be contacted", healthyBackends[0].Address)
	}
	hold.Resume()

	// Verify requests go only to healthyBackends[1] for a short time.
	shortCtx, cancel := context.WithTimeout(ctx, defaultTestShortTimeout)
	defer cancel()
	for ; shortCtx.Err() == nil; <-time.After(time.Millisecond) {
		var peer peer.Peer
		if _, err := client.EmptyCall(ctx, &testpb.Empty{}, grpc.Peer(&peer)); err != nil {
			if status.Code(err) != codes.DeadlineExceeded {
				t.Fatalf("EmptyCall() returned unexpected error %v", err)
			}
			break
		}
		if got, want := peer.Addr.String(), healthyBackends[1].Address; got != want {
			t.Fatalf("EmptyCall() went to unexpected backend: got %q, want %q", got, want)
		}
	}

	// shutdown the connected backend in endpoints[1], requests should start
	// going to the second address in the same endpoint.
	healthyBackends[1].Stop()
	if err := roundrobin.CheckRoundRobinRPCs(ctx, client, []resolver.Address{endpoints[1].Addresses[1]}); err != nil {
		t.Fatalf("RPCs didn't go to second address in the second endpoint: %v", err)
	}

	// Reduce the ejection interval and run the interval algorithm again, it
	// should uneject endpoints[0].
	odCfg.MaxEjectionTime = 0
	odCfg.BaseEjectionTime = 0
	<-time.After(time.Millisecond)
	r.UpdateState(resolver.State{Endpoints: endpoints})
	od.intervalTimerAlgorithm()
	if err := roundrobin.CheckRoundRobinRPCs(ctx, client, []resolver.Address{endpoints[0].Addresses[1], endpoints[1].Addresses[1]}); err != nil {
		t.Fatalf("RPCs didn't go to the second addresses of both endpoints: %v", err)
	}
}

// Tests that removing an address from an endpoint resets its ejection state.
// The test creates two endpoints, each with two addresses. The first endpoint
// has a backend that always returns errors. The test verifies that the first
// endpoint is ejected after running the intervalTimerAlgorithm. The test sends
// a resolver update that removes the first address in the ejected endpoint. The
// test verifies that requests start reaching the remaining address from the
// first endpoint.
func (s) TestEjectionStateResetsWhenEndpointAddressesChange(t *testing.T) {
	unhealthyBackend := &stubserver.StubServer{
		EmptyCallF: func(context.Context, *testpb.Empty) (*testpb.Empty, error) {
			return nil, errors.New("some error")
		},
	}
	if err := unhealthyBackend.StartServer(); err != nil {
		t.Fatalf("Failed to start backend: %v", err)
	}
	defer unhealthyBackend.Stop()
	t.Logf("Started unhealthy TestService backend at: %q", unhealthyBackend.Address)

	healthyBackends := make([]*stubserver.StubServer, 3)
	for i := 0; i < 3; i++ {
		healthyBackends[i] = stubserver.StartTestService(t, nil)
		defer healthyBackends[i].Stop()
	}

	wrrCfg, err := balancer.Get(weightedroundrobin.Name).(balancer.ConfigParser).ParseConfig(json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("Failed to parse %q config: %v", weightedroundrobin.Name, err)
	}
	// The interval is intentionally kept very large, the interval algorithm
	// will be triggered manually.
	odCfg := &LBConfig{
		Interval:         iserviceconfig.Duration(300 * time.Second),
		BaseEjectionTime: iserviceconfig.Duration(300 * time.Second),
		MaxEjectionTime:  iserviceconfig.Duration(300 * time.Second),
		FailurePercentageEjection: &FailurePercentageEjection{
			Threshold:             50,
			EnforcementPercentage: 100,
			MinimumHosts:          0,
			RequestVolume:         2,
		},
		MaxEjectionPercent: 100,
		ChildPolicy: &iserviceconfig.BalancerConfig{
			Name:   weightedroundrobin.Name,
			Config: wrrCfg,
		},
	}

	lbChan := make(chan *outlierDetectionBalancer, 1)
	bf := stub.BalancerFuncs{
		Init: func(bd *stub.BalancerData) {
			bd.ChildBalancer = balancer.Get(Name).Build(bd.ClientConn, bd.BuildOptions)
			lbChan <- bd.ChildBalancer.(*outlierDetectionBalancer)
		},
		Close: func(bd *stub.BalancerData) {
			bd.ChildBalancer.Close()
		},
		UpdateClientConnState: func(bd *stub.BalancerData, ccs balancer.ClientConnState) error {
			ccs.BalancerConfig = odCfg
			return bd.ChildBalancer.UpdateClientConnState(ccs)
		},
	}

	stub.Register(t.Name(), bf)
	r := manual.NewBuilderWithScheme("whatever")
	endpoints := []resolver.Endpoint{
		{
			Addresses: []resolver.Address{
				{Addr: unhealthyBackend.Address},
				{Addr: healthyBackends[0].Address},
			},
		},
		{
			Addresses: []resolver.Address{
				{Addr: healthyBackends[1].Address},
				{Addr: healthyBackends[2].Address},
			},
		},
	}

	r.InitialState(resolver.State{
		Endpoints: endpoints,
	})
	dialer := testutils.NewBlockingDialer()
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(fmt.Sprintf(`{ "loadBalancingConfig": [{%q: {}}] }`, t.Name())),
		grpc.WithResolvers(r),
		grpc.WithContextDialer(dialer.DialContext),
	}
	cc, err := grpc.NewClient(r.Scheme()+":///", opts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() failed: %v", err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	client := testgrpc.NewTestServiceClient(cc)
	client.EmptyCall(ctx, &testpb.Empty{})
	testutils.AwaitState(ctx, t, cc, connectivity.Ready)

	// Wait until both endpoints start receiving requests.
	addrsSeen := map[string]bool{}
	for ; ctx.Err() == nil && len(addrsSeen) < 2; <-time.After(time.Millisecond) {
		var peer peer.Peer
		client.EmptyCall(ctx, &testpb.Empty{}, grpc.Peer(&peer))
		addrsSeen[peer.String()] = true
	}

	if len(addrsSeen) < 2 {
		t.Fatalf("Context timed out waiting for requests to reach both endpoints.")
	}

	// Make 2 requests to each endpoint and verify the first endpoint gets
	// ejected.
	for i := 0; i < 2*len(endpoints); i++ {
		client.EmptyCall(ctx, &testpb.Empty{})
	}
	var od *outlierDetectionBalancer
	select {
	case <-ctx.Done():
		t.Fatal("Timed out waiting for the outlier detection LB policy to be built.")
	case od = <-lbChan:
	}
	od.intervalTimerAlgorithm()

	// The first endpoint should be ejected, requests should only go to
	// endpoints[1].
	if err := roundrobin.CheckRoundRobinRPCs(ctx, client, []resolver.Address{endpoints[1].Addresses[0]}); err != nil {
		t.Fatalf("RPCs didn't go to the second endpoint: %v", err)
	}

	// Remove the first address from the first endpoint. This makes the first
	// endpoint a new endpoint for outlier detection, resetting its ejection
	// status.
	r.UpdateState(resolver.State{Endpoints: []resolver.Endpoint{
		{Addresses: []resolver.Address{endpoints[0].Addresses[1]}},
		endpoints[1],
	}})
	od.intervalTimerAlgorithm()
	if err := roundrobin.CheckRoundRobinRPCs(ctx, client, []resolver.Address{endpoints[0].Addresses[1], endpoints[1].Addresses[0]}); err != nil {
		t.Fatalf("RPCs didn't go to the second addresses of both endpoints: %v", err)
	}
}
