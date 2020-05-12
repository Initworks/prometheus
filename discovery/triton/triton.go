// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package triton

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	conntrack "github.com/mwitkow/go-conntrack"
	"github.com/pkg/errors"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/discovery/refresh"
	"github.com/prometheus/prometheus/discovery/targetgroup"
)

const (
	tritonLabel             = model.MetaLabelPrefix + "triton_"
	tritonLabelGroups       = tritonLabel + "groups"
	tritonLabelMachineID    = tritonLabel + "machine_id"
	tritonLabelMachineAlias = tritonLabel + "machine_alias"
	tritonLabelMachineBrand = tritonLabel + "machine_brand"
	tritonLabelMachineImage = tritonLabel + "machine_image"
	tritonLabelServerID     = tritonLabel + "server_id"
)

// DefaultSDConfig is the default Triton SD configuration.
var DefaultSDConfig = SDConfig{
	ServerType:      "vm",
	Port:            9163,
	RefreshInterval: model.Duration(60 * time.Second),
	Version:         1,
}

// SDConfig is the configuration for Triton based service discovery.
type SDConfig struct {
	Account         string                `yaml:"account"`
	ServerType      string                `yaml:"server_type,omitempty"`
	DNSSuffix       string                `yaml:"dns_suffix"`
	Endpoint        string                `yaml:"endpoint"`
	Groups          []string              `yaml:"groups,omitempty"`
	Port            int                   `yaml:"port"`
	RefreshInterval model.Duration        `yaml:"refresh_interval,omitempty"`
	TLSConfig       config_util.TLSConfig `yaml:"tls_config,omitempty"`
	Version         int                   `yaml:"version"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	if c.ServerType != "vm" && c.ServerType != "gz" {
		return errors.New("triton SD configuration requires server_type to be 'vm' or 'gz'")
	}
	if c.Account == "" {
		return errors.New("triton SD configuration requires an account")
	}
	if c.DNSSuffix == "" {
		return errors.New("triton SD configuration requires a dns_suffix")
	}
	if c.Endpoint == "" {
		return errors.New("triton SD configuration requires an endpoint")
	}
	if c.RefreshInterval <= 0 {
		return errors.New("triton SD configuration requires RefreshInterval to be a positive integer")
	}
	return nil
}

// DiscoveryResponse models a JSON response from the Triton discovery.
type discoveryResponse struct {
	Containers []struct {
		Groups      []string `json:"groups"`
		ServerUUID  string   `json:"server_uuid"`
		VMAlias     string   `json:"vm_alias"`
		VMBrand     string   `json:"vm_brand"`
		VMImageUUID string   `json:"vm_image_uuid"`
		VMUUID      string   `json:"vm_uuid"`
	} `json:"containers"`
}

// GZDiscoveryResponse models a JSON response from the Triton discovery /gz/ endpoint.
type gzDiscoveryResponse struct {
	GZs []struct {
		ServerUUID     string `json:"server_uuid"`
		ServerHostname string `json:"server_hostname"`
	} `json:"cns"`
}

// Discovery periodically performs Triton-SD requests. It implements
// the Discoverer interface.
type Discovery struct {
	*refresh.Discovery
	client   *http.Client
	interval time.Duration
	sdConfig *SDConfig
}

// New returns a new Discovery which periodically refreshes its targets.
func New(logger log.Logger, conf *SDConfig) (*Discovery, error) {
	tls, err := config_util.NewTLSConfig(&conf.TLSConfig)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		TLSClientConfig: tls,
		DialContext: conntrack.NewDialContextFunc(
			conntrack.DialWithTracing(),
			conntrack.DialWithName("triton_sd"),
		),
	}
	client := &http.Client{Transport: transport}

	d := &Discovery{
		client:   client,
		interval: time.Duration(conf.RefreshInterval),
		sdConfig: conf,
	}
	d.Discovery = refresh.NewDiscovery(
		logger,
		"triton",
		time.Duration(conf.RefreshInterval),
		d.refresh,
	)
	return d, nil
}

func (d *Discovery) refresh(ctx context.Context) ([]*targetgroup.Group, error) {
	var endpointFormat string
	switch d.sdConfig.ServerType {
	case "vm":
		endpointFormat = "https://%s:%d/v%d/discover"
	case "gz":
		endpointFormat = "https://%s:%d/v%d/gz/discover"
	default:
		return nil, errors.New(fmt.Sprintf("unknown server_type '%s' in configuration", d.sdConfig.ServerType))
	}
	var endpoint = fmt.Sprintf(endpointFormat, d.sdConfig.Endpoint, d.sdConfig.Port, d.sdConfig.Version)
	if len(d.sdConfig.Groups) > 0 {
		groups := url.QueryEscape(strings.Join(d.sdConfig.Groups, ","))
		endpoint = fmt.Sprintf("%s?groups=%s", endpoint, groups)
	}

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "an error occurred when requesting targets from the discovery endpoint")
	}

	// Check for error responses before trying to process the body
	// Same error text is used to make TestTritonSDRefreshNoServer happy when hitting a running server on port 443 accidentally
	if (resp.StatusCode / 100) != 2 {
		return nil, errors.New("an error occurred when requesting targets from the discovery endpoint")
	}

	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "an error occurred when reading the response body")
	}

	switch d.sdConfig.ServerType {
	case "vm":
		return d.processVMResponse(data, endpoint)
	case "gz":
		return d.processGZResponse(data, endpoint)
	default:
		return nil, errors.New(fmt.Sprintf("unknown server_type '%s' in configuration", d.sdConfig.ServerType))
	}
}

func (d *Discovery) processVMResponse(data []byte, endpoint string) ([]*targetgroup.Group, error) {
	tg := &targetgroup.Group{
		Source: endpoint,
	}

	dr := discoveryResponse{}
	err := json.Unmarshal(data, &dr)
	if err != nil {
		return nil, errors.Wrap(err, "an error occurred unmarshaling the discovery response json")
	}

	for _, container := range dr.Containers {
		labels := model.LabelSet{
			tritonLabelMachineID:    model.LabelValue(container.VMUUID),
			tritonLabelMachineAlias: model.LabelValue(container.VMAlias),
			tritonLabelMachineBrand: model.LabelValue(container.VMBrand),
			tritonLabelMachineImage: model.LabelValue(container.VMImageUUID),
			tritonLabelServerID:     model.LabelValue(container.ServerUUID),
		}
		addr := fmt.Sprintf("%s.%s:%d", container.VMUUID, d.sdConfig.DNSSuffix, d.sdConfig.Port)
		labels[model.AddressLabel] = model.LabelValue(addr)

		if len(container.Groups) > 0 {
			name := "," + strings.Join(container.Groups, ",") + ","
			labels[tritonLabelGroups] = model.LabelValue(name)
		}

		tg.Targets = append(tg.Targets, labels)
	}

	return []*targetgroup.Group{tg}, nil
}

func (d *Discovery) processGZResponse(data []byte, endpoint string) ([]*targetgroup.Group, error) {
	tg := &targetgroup.Group{
		Source: endpoint,
	}

	dr := gzDiscoveryResponse{}
	err := json.Unmarshal(data, &dr)
	if err != nil {
		return nil, errors.Wrap(err, "an error occurred unmarshaling the gz discovery response json")
	}

	for _, gz := range dr.GZs {
		labels := model.LabelSet{
			tritonLabelMachineID:    model.LabelValue(gz.ServerUUID),
			tritonLabelMachineAlias: model.LabelValue(gz.ServerHostname),
			tritonLabelMachineBrand: model.LabelValue("gz"),
		}
		addr := fmt.Sprintf("%s.%s:%d", gz.ServerUUID, d.sdConfig.DNSSuffix, d.sdConfig.Port)
		labels[model.AddressLabel] = model.LabelValue(addr)

		tg.Targets = append(tg.Targets, labels)
	}

	return []*targetgroup.Group{tg}, nil
}
