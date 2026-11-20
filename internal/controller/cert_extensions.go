package controller

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	openvoxv1alpha1 "github.com/yboikov/voxkube-operator/api/v1alpha1"
)

// Puppet certificate extensions. Agents put them into their CSR from csr_attributes.yaml (extension_requests),
// and the CA copies them into the signed certificate. Puppet code sees them as $trusted['extensions'].
const (
	puppetExtensionArc   = "1.3.6.1.4.1.34380.1."
	puppetPrivateExtArc  = "1.3.6.1.4.1.34380.1.2." // ppPrivCertExt: site-specific, shown under their OID
	ppRoleExtensionName  = "pp_role"
	ppSecretPSKExtension = "pp_preshared_key"
)

// puppetExtensionNames maps Puppet's registered extension OIDs (ppRegCertExt 1.3.6.1.4.1.34380.1.1.* and
// ppAuthCertExt 1.3.6.1.4.1.34380.1.3.*) to their short names, as in Puppet's lib/puppet/ssl/oids.rb.
var puppetExtensionNames = map[string]string{
	"1.3.6.1.4.1.34380.1.1.1":  "pp_uuid",
	"1.3.6.1.4.1.34380.1.1.2":  "pp_instance_id",
	"1.3.6.1.4.1.34380.1.1.3":  "pp_image_name",
	"1.3.6.1.4.1.34380.1.1.4":  ppSecretPSKExtension,
	"1.3.6.1.4.1.34380.1.1.5":  "pp_cost_center",
	"1.3.6.1.4.1.34380.1.1.6":  "pp_product",
	"1.3.6.1.4.1.34380.1.1.7":  "pp_project",
	"1.3.6.1.4.1.34380.1.1.8":  "pp_application",
	"1.3.6.1.4.1.34380.1.1.9":  "pp_service",
	"1.3.6.1.4.1.34380.1.1.10": "pp_employee",
	"1.3.6.1.4.1.34380.1.1.11": "pp_created_by",
	"1.3.6.1.4.1.34380.1.1.12": "pp_environment",
	"1.3.6.1.4.1.34380.1.1.13": ppRoleExtensionName,
	"1.3.6.1.4.1.34380.1.1.14": "pp_software_version",
	"1.3.6.1.4.1.34380.1.1.15": "pp_department",
	"1.3.6.1.4.1.34380.1.1.16": "pp_cluster",
	"1.3.6.1.4.1.34380.1.1.17": "pp_provisioner",
	"1.3.6.1.4.1.34380.1.1.18": "pp_region",
	"1.3.6.1.4.1.34380.1.1.19": "pp_datacenter",
	"1.3.6.1.4.1.34380.1.1.20": "pp_zone",
	"1.3.6.1.4.1.34380.1.1.21": "pp_network",
	"1.3.6.1.4.1.34380.1.1.22": "pp_securitypolicy",
	"1.3.6.1.4.1.34380.1.1.23": "pp_cloudplatform",
	"1.3.6.1.4.1.34380.1.1.24": "pp_apptier",
	"1.3.6.1.4.1.34380.1.1.25": "pp_hostname",
	"1.3.6.1.4.1.34380.1.1.26": "pp_owner",
	"1.3.6.1.4.1.34380.1.3.1":  "pp_authorization",
	"1.3.6.1.4.1.34380.1.3.13": "pp_auth_role",
	"1.3.6.1.4.1.34380.1.3.39": "pp_cli_auth",
}

// PuppetExtensionsFromPEM returns the Puppet extensions (pp_*) of a PEM certificate or certificate request as
// name → value. Registered extensions use their short name (pp_role), others their OID. pp_preshared_key is a
// secret and is never returned. It returns nil when there are no Puppet extensions.
func PuppetExtensionsFromPEM(pemBytes []byte) (map[string]string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	var exts []pkix.Extension
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		exts = cert.Extensions
	case "CERTIFICATE REQUEST", "NEW CERTIFICATE REQUEST":
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return nil, err
		}
		exts = csr.Extensions
	default:
		return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
	}

	out := map[string]string{}
	for _, e := range exts {
		oid := e.Id.String()
		if !strings.HasPrefix(oid, puppetExtensionArc) {
			continue
		}
		name, ok := puppetExtensionNames[oid]
		if !ok {
			name = oid
		}
		if name == ppSecretPSKExtension {
			continue
		}
		out[name] = extensionValue(e.Value)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// extensionValue decodes a Puppet extension value. Puppet encodes them as DER UTF8String; older agents wrote raw
// bytes. Anything else is returned hex-encoded.
func extensionValue(raw []byte) string {
	var s string
	if rest, err := asn1.Unmarshal(raw, &s); err == nil && len(rest) == 0 {
		return s
	}
	if utf8.Valid(raw) && strings.IndexFunc(string(raw), func(r rune) bool { return !unicode.IsPrint(r) }) < 0 {
		return string(raw)
	}
	return "0x" + hex.EncodeToString(raw)
}

// EffectiveRole is the role passed to Puppet as the top-scope $role: spec.classification.role when set,
// otherwise the pp_role extension of the node's certificate.
func EffectiveRole(node *openvoxv1alpha1.VoxNode) string {
	if node.Spec.Classification.Role != "" {
		return node.Spec.Classification.Role
	}
	return node.Status.CertificateExtensions[ppRoleExtensionName]
}
