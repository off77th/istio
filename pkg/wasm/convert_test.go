// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wasm

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"testing"

	udpa "github.com/cncf/xds/go/udpa/type/v1"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	rbac "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	wasm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	"github.com/envoyproxy/go-control-plane/pkg/conversion"
	resource "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
	anypb "google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	extensions "istio.io/api/extensions/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/protoconv"
	"istio.io/istio/pkg/config/xds"
)

type mockCache struct {
	wantSecret []byte
	wantPolicy extensions.PullPolicy
}

func (c *mockCache) Get(downloadURL string, opts GetOptions) (string, error) {
	url, _ := url.Parse(downloadURL)
	query := url.Query()

	module := query.Get("module")
	errMsg := query.Get("error")
	var err error
	if errMsg != "" {
		err = errors.New(errMsg)
	}
	if c.wantSecret != nil && !reflect.DeepEqual(c.wantSecret, opts.PullSecret) {
		return "", fmt.Errorf("wrong secret for %v, got %q want %q", downloadURL, string(opts.PullSecret), c.wantSecret)
	}
	if c.wantPolicy != opts.PullPolicy {
		return "", fmt.Errorf("wrong pull policy for %v, got %v want %v", downloadURL, opts.PullPolicy, c.wantPolicy)
	}

	return module, err
}
func (c *mockCache) Cleanup() {}

func messageToStruct(t *testing.T, m proto.Message) *structpb.Struct {
	st, err := conversion.MessageToStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func newStruct(t *testing.T, m map[string]any) *structpb.Struct {
	st, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func messageToAnyWithTypeURL(t *testing.T, msg proto.Message, typeURL string) *anypb.Any {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return &anypb.Any{
		// nolint: staticcheck
		TypeUrl: typeURL,
		Value:   b,
	}
}

func TestWasmConvertWithWrongMessages(t *testing.T) {
	cases := []struct {
		name     string
		input    []*anypb.Any
		wantNack bool
	}{
		{
			name:     "wrong typed config",
			input:    []*anypb.Any{protoconv.MessageToAny(&emptypb.Empty{})},
			wantNack: true,
		},
		{
			name: "wrong value in typed struct",
			input: []*anypb.Any{protoconv.MessageToAny(&core.TypedExtensionConfig{
				Name: "wrong-input",
				TypedConfig: protoconv.MessageToAny(
					&udpa.TypedStruct{
						TypeUrl: xds.WasmHTTPFilterType,
						Value:   newStruct(t, map[string]any{"wrong": "value"}),
					},
				),
			})},
			wantNack: true,
		},
		{
			name: "empty wasm in typed struct",
			input: []*anypb.Any{protoconv.MessageToAny(&core.TypedExtensionConfig{
				Name: "wrong-input",
				TypedConfig: protoconv.MessageToAny(
					&udpa.TypedStruct{
						TypeUrl: xds.WasmHTTPFilterType,
						Value:   messageToStruct(t, &wasm.Wasm{}),
					},
				),
			})},
			wantNack: true,
		},
		{
			name: "no remote and local code in wasm",
			input: []*anypb.Any{protoconv.MessageToAny(&core.TypedExtensionConfig{
				Name: "wrong-input",
				TypedConfig: protoconv.MessageToAny(
					&udpa.TypedStruct{
						TypeUrl: xds.WasmHTTPFilterType,
						Value: messageToStruct(t, &wasm.Wasm{
							Config: &v3.PluginConfig{
								Vm: &v3.PluginConfig_VmConfig{
									VmConfig: &v3.VmConfig{
										Code: &core.AsyncDataSource{},
									},
								},
							},
						}),
					},
				),
			})},
			wantNack: true,
		},
		{
			name: "empty wasm in typed extension config",
			input: []*anypb.Any{protoconv.MessageToAny(&core.TypedExtensionConfig{
				Name:        "wrong-input",
				TypedConfig: protoconv.MessageToAny(&wasm.Wasm{}),
			})},
			wantNack: true,
		},
		{
			name: "wrong wasm filter value",
			input: []*anypb.Any{protoconv.MessageToAny(&core.TypedExtensionConfig{
				Name:        "wrong-input",
				TypedConfig: messageToAnyWithTypeURL(t, &v3.VmConfig{Runtime: "test", VmId: "test"}, xds.WasmHTTPFilterType),
			})},
			wantNack: true,
		},
		{
			name: "wrong typed struct value",
			input: []*anypb.Any{protoconv.MessageToAny(&core.TypedExtensionConfig{
				Name:        "wrong-input",
				TypedConfig: messageToAnyWithTypeURL(t, &v3.VmConfig{Runtime: "test", VmId: "test"}, xds.TypedStructType),
			})},
			wantNack: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantNack := true // Nack should be returned due to the wrong input.
			mc := &mockCache{}
			gotNack := MaybeConvertWasmExtensionConfig(tc.input, mc)
			if gotNack != wantNack {
				t.Errorf("wasm config conversion send nack got %v want %v", gotNack, tc.wantNack)
			}
		})
	}
}

func TestWasmConvert(t *testing.T) {
	cases := []struct {
		name       string
		input      []*core.TypedExtensionConfig
		wantOutput []*core.TypedExtensionConfig
		wantNack   bool
	}{
		{
			name: "nil typed config ",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["nil-typed-config"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["nil-typed-config"],
			},
			wantNack: true,
		},
		{
			name: "remote load success",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-success"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-success-local-file"],
			},
			wantNack: false,
		},
		{
			name: "remote load success without typed struct",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-success-without-typed-struct"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-success-local-file"],
			},
			wantNack: false,
		},
		{
			name: "remote load fail",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-fail"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-fail"],
			},
			wantNack: true,
		},
		{
			name: "mix",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-fail"],
				extensionConfigMap["remote-load-success"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-fail"],
				extensionConfigMap["remote-load-success-local-file"],
			},
			wantNack: true,
		},
		{
			name: "remote load fail open",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-fail-open"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-allow"],
			},
			wantNack: false,
		},
		{
			name: "no typed struct",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["empty"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["empty"],
			},
			wantNack: false,
		},
		{
			name: "no wasm",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["no-wasm"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["no-wasm"],
			},
			wantNack: false,
		},
		{
			name: "no remote load",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["no-remote-load"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["no-remote-load"],
			},
			wantNack: false,
		},
		{
			name: "no uri",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["no-http-uri"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["no-http-uri"],
			},
			wantNack: true,
		},
		{
			name: "secret",
			input: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-secret"],
			},
			wantOutput: []*core.TypedExtensionConfig{
				extensionConfigMap["remote-load-success-local-file"],
			},
			wantNack: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resources := make([]*anypb.Any, 0, len(c.input))
			for _, i := range c.input {
				resources = append(resources, protoconv.MessageToAny(i))
			}
			mc := &mockCache{}
			gotNack := MaybeConvertWasmExtensionConfig(resources, mc)
			if len(resources) != len(c.wantOutput) {
				t.Fatalf("wasm config conversion number of configuration got %v want %v", len(resources), len(c.wantOutput))
			}
			for i, output := range resources {
				ec := &core.TypedExtensionConfig{}
				if err := output.UnmarshalTo(ec); err != nil {
					t.Errorf("wasm config conversion output %v failed to unmarshal", output)
					continue
				}
				if !proto.Equal(ec, c.wantOutput[i]) {
					t.Errorf("wasm config conversion output index %d got %v want %v", i, ec, c.wantOutput[i])
				}
			}
			if gotNack != c.wantNack {
				t.Errorf("wasm config conversion send nack got %v want %v", gotNack, c.wantNack)
			}
		})
	}
}

func buildTypedStructExtensionConfig(name string, wasm *wasm.Wasm) *core.TypedExtensionConfig {
	ws, _ := conversion.MessageToStruct(wasm)
	return &core.TypedExtensionConfig{
		Name: name,
		TypedConfig: protoconv.MessageToAny(
			&udpa.TypedStruct{
				TypeUrl: xds.WasmHTTPFilterType,
				Value:   ws,
			},
		),
	}
}

func buildAnyExtensionConfig(name string, msg proto.Message) *core.TypedExtensionConfig {
	return &core.TypedExtensionConfig{
		Name:        name,
		TypedConfig: protoconv.MessageToAny(msg),
	}
}

var extensionConfigMap = map[string]*core.TypedExtensionConfig{
	"nil-typed-config": {
		Name:        "nil-typed-config",
		TypedConfig: nil,
	},
	"empty": {
		Name: "empty",
		TypedConfig: protoconv.MessageToAny(
			&structpb.Struct{},
		),
	},
	"no-wasm": {
		Name: "no-wasm",
		TypedConfig: protoconv.MessageToAny(
			&udpa.TypedStruct{TypeUrl: resource.APITypePrefix + "sometype"},
		),
	},
	"no-remote-load": buildTypedStructExtensionConfig("no-remote-load", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Runtime: "envoy.wasm.runtime.null",
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Local{
						Local: &core.DataSource{
							Specifier: &core.DataSource_InlineString{
								InlineString: "envoy.wasm.metadata_exchange",
							},
						},
					}},
				},
			},
		},
	}),
	"no-http-uri": buildTypedStructExtensionConfig("no-remote-load", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Remote{
						Remote: &core.RemoteDataSource{},
					}},
				},
			},
		},
	}),
	"remote-load-success": buildTypedStructExtensionConfig("remote-load-success", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Remote{
						Remote: &core.RemoteDataSource{
							HttpUri: &core.HttpUri{
								Uri: "http://test?module=test.wasm",
							},
						},
					}},
				},
			},
		},
	}),
	"remote-load-success-local-file": buildAnyExtensionConfig("remote-load-success", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Local{
						Local: &core.DataSource{
							Specifier: &core.DataSource_Filename{
								Filename: "test.wasm",
							},
						},
					}},
				},
			},
		},
	}),
	"remote-load-success-without-typed-struct": buildAnyExtensionConfig("remote-load-success", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Remote{
						Remote: &core.RemoteDataSource{
							HttpUri: &core.HttpUri{
								Uri: "http://test?module=test.wasm",
							},
						},
					}},
				},
			},
		},
	}),
	"remote-load-fail": buildTypedStructExtensionConfig("remote-load-fail", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Remote{
						Remote: &core.RemoteDataSource{
							HttpUri: &core.HttpUri{
								Uri: "http://test?module=test.wasm&error=download-error",
							},
						},
					}},
				},
			},
		},
	}),
	"remote-load-fail-open": buildTypedStructExtensionConfig("remote-load-fail", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Remote{
						Remote: &core.RemoteDataSource{
							HttpUri: &core.HttpUri{
								Uri: "http://test?module=test.wasm&error=download-error",
							},
						},
					}},
				},
			},
			FailOpen: true,
		},
	}),
	"remote-load-allow": buildAnyExtensionConfig("remote-load-fail", &rbac.RBAC{}),
	"remote-load-secret": buildTypedStructExtensionConfig("remote-load-success", &wasm.Wasm{
		Config: &v3.PluginConfig{
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Code: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Remote{
						Remote: &core.RemoteDataSource{
							HttpUri: &core.HttpUri{
								Uri: "http://test?module=test.wasm",
							},
						},
					}},
					EnvironmentVariables: &v3.EnvironmentVariables{
						KeyValues: map[string]string{
							model.WasmSecretEnv: "secret",
						},
					},
				},
			},
		},
	}),
}
