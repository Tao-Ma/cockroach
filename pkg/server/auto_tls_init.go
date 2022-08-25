// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// TODO(aaron-crl): This uses the Locator from the security package
// Getting about half way to integration with the certificate manager
// While I'd originally hoped to decouple it completely, I realized
// it would create an even larger headache if we maintained default
// certificate locations in multiple places.

package server

import (
	"context"
	"encoding/pem"
	"os"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/security/certnames"
	"github.com/cockroachdb/cockroach/pkg/security/securityassets"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/netutil/addr"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
	"github.com/cockroachdb/logtags"
)

// TODO(aaron-crl): This is an exact copy from `pkg/cli/cert.go` and should
// be refactored to share consts.
// We use 366 days on certificate lifetimes to at least match X years,
// otherwise leap years risk putting us just under.
const defaultCALifetime = 10 * 366 * 24 * time.Hour  // ten years
const defaultCertLifetime = 5 * 366 * 24 * time.Hour // five years

// Service Name Strings for autogenerated certificates.
const serviceNameInterNode = "cockroach-node"
const serviceNameUserAuth = "cockroach-client"
const serviceNameSQL = "cockroach-sql"
const serviceNameRPC = "cockroach-rpc"
const serviceNameUI = "cockroach-http"

// CertificateBundle manages the collection of certificates used by a
// CockroachDB node.
type CertificateBundle struct {
	InterNode      ServiceCertificateBundle
	UserAuth       ServiceCertificateBundle
	SQLService     ServiceCertificateBundle
	RPCService     ServiceCertificateBundle
	AdminUIService ServiceCertificateBundle
}

// ServiceCertificateBundle is a container for the CA and host node certs.
type ServiceCertificateBundle struct {
	CACertificate   []byte
	CAKey           []byte
	HostCertificate []byte // This will be blank if unused (in the user case).
	HostKey         []byte // This will be blank if unused (in the user case).
}

// Helper function to load cert and key for a service.
func (sb *ServiceCertificateBundle) loadServiceCertAndKey(
	certPath string, keyPath string,
) (err error) {
	sb.HostCertificate, err = loadCertificateFile(certPath)
	if err != nil {
		return
	}
	sb.HostKey, err = loadKeyFile(keyPath)
	if err != nil {
		return
	}
	return
}

// Helper function to load cert and key for a service CA.
func (sb *ServiceCertificateBundle) loadCACertAndKey(certPath string, keyPath string) (err error) {
	sb.CACertificate, err = loadCertificateFile(certPath)
	if err != nil {
		return
	}
	sb.CAKey, err = loadKeyFile(keyPath)
	if err != nil {
		return
	}
	return
}

// loadOrCreateServiceCertificates will attempt to load the service cert/key
// into the service bundle.
// * If they do not exist:
//   It will attempt to load the service CA cert/key pair.
//   * If they do not exist:
//     It will generate the service CA cert/key pair.
//     It will persist these to disk and store them
//       in the ServiceCertificateBundle.
//   It will generate the service cert/key pair.
//   It will persist these to disk and store them
//     in the ServiceCertificateBundle.
func (sb *ServiceCertificateBundle) loadOrCreateServiceCertificates(
	ctx context.Context,
	serviceCertPath string,
	serviceKeyPath string,
	caCertPath string,
	caKeyPath string,
	serviceCertLifespan time.Duration,
	caCertLifespan time.Duration,
	commonName string,
	serviceName string,
	hostnames []string,
	serviceCertIsAlsoValidAsClient bool,
) error {
	ctx = logtags.AddTag(ctx, "service", serviceName)

	var err error
	log.Ops.Infof(ctx, "attempting to load service cert: %s", serviceCertPath)
	// Check if the service cert and key already exist, if it does return early.
	sb.HostCertificate, err = loadCertificateFile(serviceCertPath)
	if err == nil {
		log.Ops.Infof(ctx, "found; loading service key: %s", serviceKeyPath)
		// Cert file exists, now load key.
		sb.HostKey, err = loadKeyFile(serviceKeyPath)
		if err != nil {
			// Check if we failed to load the key?
			if oserror.IsNotExist(err) {
				// Cert exists but key doesn't, this is an error.
				return errors.Wrapf(err,
					"failed to load service certificate key for %q expected key at %q",
					serviceCertPath, serviceKeyPath)
			}
			return errors.Wrap(err, "something went wrong loading service key")
		}
		// Both certificate and key should be successfully loaded.
		log.Ops.Infof(ctx, "service cert is ready")
		return nil
	}
	// TODO(aaron-crl, knz): err != nil is not handled here.

	log.Ops.Infof(ctx, "not found; will attempt auto-creation")

	log.Ops.Infof(ctx, "attempting to load CA cert: %s", caCertPath)
	// Neither service cert or key exist, attempt to load CA.
	sb.CACertificate, err = loadCertificateFile(caCertPath)
	if err == nil {
		// CA cert has been successfully loaded, attempt to load
		// CA key.
		log.Ops.Infof(ctx, "found; loading CA key: %s", caKeyPath)
		sb.CAKey, err = loadKeyFile(caKeyPath)
		if err != nil {
			return errors.Wrapf(
				err, "loaded service CA cert but failed to load service CA key file: %q", caKeyPath,
			)
		}
	} else if oserror.IsNotExist(err) {
		log.Ops.Infof(ctx, "not found; CA cert does not exist, auto-creating")
		// CA cert does not yet exist, create it and its key.
		if err := sb.createServiceCA(ctx, caCertPath, caKeyPath, caCertLifespan); err != nil {
			return errors.Wrap(
				err, "failed to create Service CA",
			)
		}
	}
	// TODO(aaron-crl, knz): missing `else` case here.

	// CA cert and key should now be loaded, create service cert and key.
	var hostCert, hostKey *pem.Block
	caCertPEM, err := security.PEMToCertificates(sb.CACertificate)
	if err != nil {
		return errors.Wrap(err, "error when decoding PEM CACertificate")
	}
	caKeyPEM, rest := pem.Decode(sb.CAKey)
	if len(rest) > 0 || caKeyPEM == nil {
		return errors.New("error when decoding PEM CAKey")
	}
	hostCert, hostKey, err = security.CreateServiceCertAndKey(
		ctx,
		log.Ops.Infof,
		serviceCertLifespan,
		commonName,
		hostnames,
		caCertPEM[0], caKeyPEM,
		serviceCertIsAlsoValidAsClient,
	)
	if err != nil {
		return errors.Wrap(
			err, "failed to create Service Cert and Key",
		)
	}
	sb.HostCertificate = pem.EncodeToMemory(hostCert)
	sb.HostKey = pem.EncodeToMemory(hostKey)

	log.Ops.Infof(ctx, "writing service cert: %s", serviceCertPath)
	if err := writeCertificateFile(serviceCertPath, hostCert, false); err != nil {
		return err
	}

	log.Ops.Infof(ctx, "writing service key: %s", serviceKeyPath)
	if err := writeKeyFile(serviceKeyPath, hostKey, false); err != nil {
		return err
	}

	return nil
}

// createServiceCA builds CA cert and key and populates them to
// ServiceCertificateBundle.
func (sb *ServiceCertificateBundle) createServiceCA(
	ctx context.Context, caCertPath string, caKeyPath string, initLifespan time.Duration,
) error {
	ctx = logtags.AddTag(ctx, "auto-create-ca", nil)

	var err error
	var caCert, caKey *pem.Block
	caCert, caKey, err = security.CreateCACertAndKey(ctx, log.Ops.Infof, initLifespan, caCommonName)
	if err != nil {
		return err
	}
	sb.CACertificate = pem.EncodeToMemory(caCert)
	sb.CAKey = pem.EncodeToMemory(caKey)

	log.Ops.Infof(ctx, "writing CA cert: %s", caCertPath)
	if err := writeCertificateFile(caCertPath, caCert, false); err != nil {
		return err
	}

	log.Ops.Infof(ctx, "writing CA key: %s", caKeyPath)
	if err := writeKeyFile(caKeyPath, caKey, false); err != nil {
		return err
	}

	return nil
}

// Simple wrapper to make it easier to store certs somewhere else later.
// TODO (aaron-crl): Put validation checks here.
func loadCertificateFile(certPath string) (cert []byte, err error) {
	cert, err = os.ReadFile(certPath)
	return
}

// Simple wrapper to make it easier to store certs somewhere else later.
// TODO (aaron-crl): Put validation checks here.
func loadKeyFile(keyPath string) (key []byte, err error) {
	key, err = os.ReadFile(keyPath)
	return
}

// Simple wrapper to make it easier to store certs somewhere else later.
// Unless overwrite is true, this function will error if a file already exists
// at certFilePath.
// TODO(aaron-crl): This was lifted from 'pkg/security' and modified. It might
// make sense to refactor these calls back to 'pkg/security' rather than
// maintain these functions.
func writeCertificateFile(certFilePath string, certificatePEM *pem.Block, overwrite bool) error {
	// Validate that we are about to write a cert. And reshape for common
	// security.WritePEMToFile().
	// TODO(aaron-crl): Validate this is actually a cert.

	// TODO(aaron-crl): Add logging here.
	return security.WritePEMToFile(certFilePath, 0644, overwrite, certificatePEM)
}

// Simple wrapper to make it easier to store certs somewhere else later.
// Unless overwrite is true, this function will error if a file already exists
// at keyFilePath.
// TODO(aaron-crl): This was lifted from 'pkg/security' and modified. It might
// make sense to refactor these calls back to 'pkg/security' rather than
// maintain these functions.
func writeKeyFile(keyFilePath string, keyPEM *pem.Block, overwrite bool) error {
	// Validate that we are about to write a key and reshape for common
	// security.WritePEMToFile().
	// TODO(aaron-crl): Validate this is actually a key.
	// TODO(aaron-crl): Add logging here.
	return security.WritePEMToFile(keyFilePath, 0600, overwrite, keyPEM)
}

// InitializeFromConfig is called by the node creating certificates for the
// cluster. It uses or generates an InterNode CA to produce any missing
// unmanaged certificates. It does this base on the logic in:
// https://github.com/cockroachdb/cockroach/pull/51991
// N.B.: This function fast fails if an inter-node cert/key pair are present
// as this should _never_ happen.
func (b *CertificateBundle) InitializeFromConfig(ctx context.Context, c base.Config) error {
	cl := certnames.MakeLocator(c.SSLCertsDir)

	// First check to see if host cert is already present
	// if it is, we should fail to initialize.
	loader := securityassets.GetLoader()
	if exists, err := loader.FileExists(cl.NodeCertPath()); err != nil {
		return err
	} else if exists {
		return errors.New("inter-node certificate already present")
	}

	rpcAddrs := extractHosts(c.Addr, c.AdvertiseAddr)
	sqlAddrs := rpcAddrs
	if c.SplitListenSQL {
		sqlAddrs = extractHosts(c.SQLAddr, c.SQLAdvertiseAddr)
	}
	httpAddrs := extractHosts(c.HTTPAddr, c.HTTPAdvertiseAddr)

	// Create the target directory if it does not exist yet.
	if err := cl.EnsureCertsDirectory(); err != nil {
		return err
	}

	// Start by loading or creating the InterNode certificates.
	if err := b.InterNode.loadOrCreateServiceCertificates(
		ctx,
		cl.NodeCertPath(),
		cl.NodeKeyPath(),
		cl.CACertPath(),
		cl.CAKeyPath(),
		defaultCertLifetime,
		defaultCALifetime,
		username.NodeUser,
		serviceNameInterNode,
		rpcAddrs,
		true, /* serviceCertIsAlsoValidAsClient */
	); err != nil {
		return errors.Wrap(err,
			"failed to load or create InterNode certificates")
	}

	// Initialize User auth certificates.
	if err := b.UserAuth.loadOrCreateServiceCertificates(
		ctx,
		cl.ClientNodeCertPath(),
		cl.ClientNodeKeyPath(),
		cl.ClientCACertPath(),
		cl.ClientCAKeyPath(),
		defaultCertLifetime,
		defaultCALifetime,
		username.NodeUser,
		serviceNameUserAuth,
		nil,
		true, /* serviceCertIsAlsoValidAsClient */
	); err != nil {
		return errors.Wrap(err,
			"failed to load or create User auth certificate(s)")
	}

	// Initialize SQLService Certs.
	if err := b.SQLService.loadOrCreateServiceCertificates(
		ctx,
		cl.SQLServiceCertPath(),
		cl.SQLServiceKeyPath(),
		cl.SQLServiceCACertPath(),
		cl.SQLServiceCAKeyPath(),
		defaultCertLifetime,
		defaultCALifetime,
		username.NodeUser,
		serviceNameSQL,
		// TODO(aaron-crl): Add RPC variable to config or SplitSQLAddr.
		sqlAddrs,
		false, /* serviceCertIsAlsoValidAsClient */
	); err != nil {
		return errors.Wrap(err,
			"failed to load or create SQL service certificate(s)")
	}

	// Initialize RPCService Certs.
	if err := b.RPCService.loadOrCreateServiceCertificates(
		ctx,
		cl.RPCServiceCertPath(),
		cl.RPCServiceKeyPath(),
		cl.RPCServiceCACertPath(),
		cl.RPCServiceCAKeyPath(),
		defaultCertLifetime,
		defaultCALifetime,
		username.NodeUser,
		serviceNameRPC,
		// TODO(aaron-crl): Add RPC variable to config.
		rpcAddrs,
		false, /* serviceCertIsAlsoValidAsClient */
	); err != nil {
		return errors.Wrap(err,
			"failed to load or create RPC service certificate(s)")
	}

	// Initialize AdminUIService Certs.
	if err := b.AdminUIService.loadOrCreateServiceCertificates(
		ctx,
		cl.UICertPath(),
		cl.UIKeyPath(),
		cl.UICACertPath(),
		cl.UICAKeyPath(),
		defaultCertLifetime,
		defaultCALifetime,
		httpAddrs[0],
		serviceNameUI,
		httpAddrs,
		false, /* serviceCertIsAlsoValidAsClient */
	); err != nil {
		return errors.Wrap(err,
			"failed to load or create Admin UI service certificate(s)")
	}

	return nil
}

func extractHosts(addrs ...string) []string {
	res := make([]string, 0, len(addrs))

	for _, a := range addrs {
		hostname, _, err := addr.SplitHostPort(a, "0")
		if err != nil {
			panic(err)
		}
		found := false
		for _, h := range res {
			if h == hostname {
				found = true
				break
			}
		}
		if !found {
			res = append(res, hostname)
		}
	}
	return res
}

// InitializeNodeFromBundle uses the contents of the CertificateBundle and
// details from the config object to write certs to disk and generate any
// missing host-specific certificates and keys
// It is assumed that a node receiving this has not has TLS initialized. If
// an inter-node certificate is found, this function will error.
func (b *CertificateBundle) InitializeNodeFromBundle(ctx context.Context, c base.Config) error {
	cl := certnames.MakeLocator(c.SSLCertsDir)

	// First check to see if host cert is already present
	// if it is, we should fail to initialize.
	loader := securityassets.GetLoader()
	if exists, err := loader.FileExists(cl.NodeCertPath()); err != nil {
		return err
	} else if exists {
		return errors.New("inter-node certificate already present")
	}

	if err := cl.EnsureCertsDirectory(); err != nil {
		return err
	}

	// Write received CA's to disk. If any of them already exist, fail
	// and return an error.

	// Attempt to write InterNodeHostCA to disk first.
	if err := b.InterNode.writeCAOrFail(cl.CACertPath(), cl.CAKeyPath()); err != nil {
		return errors.Wrap(err, "failed to write InterNodeCA to disk")
	}

	// Attempt to write ClientCA to disk.
	if err := b.UserAuth.writeCAOrFail(cl.ClientCACertPath(), cl.ClientCAKeyPath()); err != nil {
		return errors.Wrap(err, "failed to write ClientCA to disk")
	}

	// Attempt to write SQLServiceCA to disk.
	if err := b.SQLService.writeCAOrFail(cl.SQLServiceCACertPath(), cl.SQLServiceCAKeyPath()); err != nil {
		return errors.Wrap(err, "failed to write SQLServiceCA to disk")
	}

	// Attempt to write RPCServiceCA to disk.
	if err := b.RPCService.writeCAOrFail(cl.RPCServiceCACertPath(), cl.RPCServiceCAKeyPath()); err != nil {
		return errors.Wrap(err, "failed to write RPCServiceCA to disk")
	}

	// Attempt to write AdminUIServiceCA to disk.
	if err := b.AdminUIService.writeCAOrFail(cl.UICACertPath(), cl.UICAKeyPath()); err != nil {
		return errors.Wrap(err, "failed to write AdminUIServiceCA to disk")
	}

	// Once CAs are written call the same InitFromConfig function to create
	// host certificates.
	if err := b.InitializeFromConfig(ctx, c); err != nil {
		return errors.Wrap(
			err,
			"failed to initialize host certs after writing CAs to disk")
	}

	return nil
}

// writeCAOrFail will attempt to write a service certificate bundle to the
// specified paths on disk. It will ignore any missing certificate fields but
// error if it fails to write a file to disk.
func (sb *ServiceCertificateBundle) writeCAOrFail(certPath string, keyPath string) (err error) {
	caCertPEM, _ := pem.Decode(sb.CACertificate)
	if sb.CACertificate != nil {
		err = writeCertificateFile(certPath, caCertPEM, false)
		if err != nil {
			return
		}
	}

	caKeyPEM, _ := pem.Decode(sb.CAKey)
	if sb.CAKey != nil {
		err = writeKeyFile(keyPath, caKeyPEM, false)
		if err != nil {
			return
		}
	}

	return
}

func (sb *ServiceCertificateBundle) loadCACertAndKeyIfExists(
	certPath string, keyPath string,
) error {
	// TODO(aaron-crl): Possibly add a warning to the log that a CA was not
	// found.
	err := sb.loadCACertAndKey(certPath, keyPath)
	if oserror.IsNotExist(err) {
		return nil
	}
	return err
}

// collectLocalCABundle will load any CA certs and keys present on disk. It
// will skip any CA's where the certificate is not found. Any other read errors
// including permissions result in an error.
func collectLocalCABundle(SSLCertsDir string) (CertificateBundle, error) {
	cl := certnames.MakeLocator(SSLCertsDir)
	var b CertificateBundle
	var err error

	err = b.InterNode.loadCACertAndKeyIfExists(cl.CACertPath(), cl.CAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading InterNode CA cert and/or key")
	}

	err = b.UserAuth.loadCACertAndKeyIfExists(
		cl.ClientCACertPath(), cl.ClientCAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading UserAuth CA cert and/or key")
	}

	err = b.SQLService.loadCACertAndKeyIfExists(
		cl.SQLServiceCACertPath(), cl.SQLServiceCAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading SQL CA cert and/or key")
	}
	err = b.RPCService.loadCACertAndKeyIfExists(
		cl.RPCServiceCACertPath(), cl.RPCServiceCAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading RPC CA cert and/or key")
	}

	err = b.AdminUIService.loadCACertAndKeyIfExists(
		cl.UICACertPath(), cl.UICAKeyPath())
	if err != nil {
		return b, errors.Wrap(
			err, "error loading AdminUI CA cert and/or key")
	}

	return b, nil
}

// rotateGeneratedCertsOnDisk will generate and replace interface certificates
// where a corresponding CA cert and key are found. This function does not
// restart any services or cause the node to restart. That must be triggered
// after this function is successfully run.
// Service certs are written as they are generated but will return on first
// error. This is not seen as harmful as the rotation command may be rerun
// manually after rotation errors are corrected without negatively impacting
// any interface. All existing interfaces will again receive a new
// certificate/key pair.
func rotateGeneratedCerts(ctx context.Context, c base.Config) error {
	cl := certnames.MakeLocator(c.SSLCertsDir)

	// Fail fast if we can't load the CAs.
	b, err := collectLocalCABundle(c.SSLCertsDir)
	if err != nil {
		return errors.Wrap(
			err, "failed to load local CAs for certificate rotation")
	}

	rpcAddrs := extractHosts(c.Addr, c.AdvertiseAddr)
	sqlAddrs := rpcAddrs
	if c.SplitListenSQL {
		sqlAddrs = extractHosts(c.SQLAddr, c.SQLAdvertiseAddr)
	}
	httpAddrs := extractHosts(c.HTTPAddr, c.HTTPAdvertiseAddr)

	// Rotate InterNode Certs.
	if b.InterNode.CACertificate != nil {
		err = b.InterNode.rotateServiceCert(
			ctx,
			cl.NodeCertPath(),
			cl.NodeKeyPath(),
			defaultCertLifetime,
			username.NodeUser,
			serviceNameInterNode,
			rpcAddrs,
			true, /* serviceCertIsAlsoValidAsClient */
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate InterNode cert")
		}
	}

	// Rotate UserAuth certificate
	if b.UserAuth.CACertificate != nil {
		err = b.UserAuth.rotateServiceCert(
			ctx,
			cl.ClientNodeCertPath(),
			cl.ClientNodeKeyPath(),
			defaultCertLifetime,
			username.NodeUser,
			serviceNameUserAuth,
			nil,
			true, /* serviceCertIsAlsoValidAsClient */
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate InterNode cert")
		}
	}

	// Rotate SQLService Certs.
	if b.SQLService.CACertificate != nil {
		err = b.SQLService.rotateServiceCert(
			ctx,
			cl.SQLServiceCertPath(),
			cl.SQLServiceKeyPath(),
			defaultCertLifetime,
			username.NodeUser,
			serviceNameSQL,
			sqlAddrs,
			false, /* serviceCertIsAlsoValidAsClient */
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate SQLService cert")
		}
	}

	// Rotate RPCService Certs.
	if b.RPCService.CACertificate != nil {
		err = b.RPCService.rotateServiceCert(
			ctx,
			cl.RPCServiceCertPath(),
			cl.RPCServiceKeyPath(),
			defaultCertLifetime,
			username.NodeUser,
			serviceNameRPC,
			rpcAddrs,
			false, /* serviceCertIsAlsoValidAsClient */
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate RPCService cert")
		}
	}

	// Rotate AdminUIService Certs.
	if b.AdminUIService.CACertificate != nil {
		err = b.AdminUIService.rotateServiceCert(
			ctx,
			cl.UICertPath(),
			cl.UIKeyPath(),
			defaultCertLifetime,
			httpAddrs[0],
			serviceNameUI,
			httpAddrs,
			false, /* serviceCertIsAlsoValidAsClient */
		)
		if err != nil {
			return errors.Wrap(err, "failed to rotate AdminUIService cert")
		}
	}

	return nil
}

// rotateServiceCert will generate a new service certificate for the provided
// hostnames and path signed by the ca at the supplied paths. It will only
// succeed if it is able to generate these and OVERWRITE an exist file.
func (sb *ServiceCertificateBundle) rotateServiceCert(
	ctx context.Context,
	certPath string,
	keyPath string,
	serviceCertLifespan time.Duration,
	commonName, serviceString string,
	hostnames []string,
	serviceCertIsAlsoValidAsClient bool,
) error {
	// generate
	caCertPEM, err := security.PEMToCertificates(sb.CACertificate)
	if err != nil {
		return errors.Wrap(err, "error when decoding PEM CACertificate")
	}
	caKeyPEM, rest := pem.Decode(sb.CAKey)
	if len(rest) > 0 || caKeyPEM == nil {
		return errors.New("error when decoding PEM CAKey")
	}
	certPEM, keyPEM, err := security.CreateServiceCertAndKey(
		ctx,
		log.Ops.Infof,
		serviceCertLifespan,
		commonName,
		hostnames,
		caCertPEM[0],
		caKeyPEM,
		serviceCertIsAlsoValidAsClient,
	)
	if err != nil {
		return errors.Wrapf(
			err, "failed to rotate certs for %q", serviceString)
	}

	// Check to make sure we're about to overwrite a file.
	if _, err := os.Stat(certPath); err == nil {
		err = writeCertificateFile(certPath, certPEM, true)
		if err != nil {
			return errors.Wrapf(
				err, "failed to rotate certs for %q", serviceString)
		}
	} else {
		return errors.Wrapf(
			err, "failed to rotate certs for %q", serviceString)
	}

	// Check to make sure we're about to overwrite a file.
	if _, err := os.Stat(certPath); err == nil {
		err = writeKeyFile(keyPath, keyPEM, true)
		if err != nil {
			return errors.Wrapf(
				err, "failed to rotate certs for %q", serviceString)
		}
	} else {
		return errors.Wrapf(
			err, "failed to rotate certs for %q", serviceString)
	}

	return nil
}
