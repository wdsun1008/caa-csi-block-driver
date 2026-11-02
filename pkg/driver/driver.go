// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

var drvLogger = log.New(log.Writer(), "[caa-csi/driver] ", log.LstdFlags|log.Lmsgprefix)

type Config struct {
	DirectVolumeMetadataVersion string
	Endpoint                    string
	DriverName                  string
	VendorVersion               string
	NodeID                      string
	Component                   string
	VolumeStoreDir              string
}

type Driver struct {
	config Config
	server *grpc.Server
	store  *volumeStore
}

func NewDriver(cfg Config) (*Driver, error) {
	if cfg.DriverName == "" {
		return nil, fmt.Errorf("driver name is required")
	}
	if cfg.VendorVersion == "" {
		return nil, fmt.Errorf("driver version is required")
	}
	if cfg.Component == "" {
		cfg.Component = "all"
	}
	if cfg.Component != "all" && cfg.Component != "controller" && cfg.Component != "node" {
		return nil, fmt.Errorf("component must be all, controller, or node")
	}
	if (cfg.Component == "all" || cfg.Component == "node") && cfg.NodeID == "" {
		return nil, fmt.Errorf("node ID is required")
	}

	if cfg.DirectVolumeMetadataVersion == "" {
		cfg.DirectVolumeMetadataVersion = "legacy"
	}
	if cfg.DirectVolumeMetadataVersion != "legacy" && cfg.DirectVolumeMetadataVersion != "v1" {
		return nil, fmt.Errorf("direct-volume metadata version must be legacy or v1")
	}
	driver := &Driver{config: cfg}
	if cfg.Component == "all" || cfg.Component == "controller" {
		var err error
		if cfg.VolumeStoreDir == "" {
			driver.store, err = newVolumeStore()
		} else {
			driver.store, err = newVolumeStoreAt(cfg.VolumeStoreDir)
		}
		if err != nil {
			return nil, err
		}
	}

	return driver, nil
}

func (d *Driver) Run() error {
	scheme, addr, err := parseEndpoint(d.config.Endpoint)
	if err != nil {
		return err
	}

	if scheme == "unix" {
		os.Remove(addr)
	}

	listener, err := net.Listen(scheme, addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s://%s: %w", scheme, addr, err)
	}

	d.server = grpc.NewServer()
	d.registerServices(d.server)

	drvLogger.Printf("Listening on %s://%s (driver=%s, version=%s, component=%s, node=%s)",
		scheme, addr, d.config.DriverName, d.config.VendorVersion, d.config.Component, d.config.NodeID)

	return d.server.Serve(listener)
}

func (d *Driver) registerServices(server *grpc.Server) {
	controller := d.config.Component == "all" || d.config.Component == "controller"
	csi.RegisterIdentityServer(server, &identityServer{
		driverName:    d.config.DriverName,
		vendorVersion: d.config.VendorVersion,
		controller:    controller,
	})
	if controller {
		csi.RegisterControllerServer(server, newControllerServer(d.store))
	}
	if d.config.Component == "all" || d.config.Component == "node" {
		node := newNodeServer(d.config.NodeID)
		node.versionedMetadata = d.config.DirectVolumeMetadataVersion == "v1"
		csi.RegisterNodeServer(server, node)
	}
}

func (d *Driver) Stop() {
	if d.server != nil {
		d.server.GracefulStop()
	}
}

func parseEndpoint(endpoint string) (string, string, error) {
	if strings.HasPrefix(endpoint, "unix://") {
		return "unix", strings.TrimPrefix(endpoint, "unix://"), nil
	}
	if strings.HasPrefix(endpoint, "tcp://") {
		return "tcp", strings.TrimPrefix(endpoint, "tcp://"), nil
	}
	return "", "", fmt.Errorf("unsupported endpoint scheme: %s", endpoint)
}
