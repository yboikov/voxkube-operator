package controller

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeCSR builds a CSR with Ruby's OpenSSL: an optional challengePassword attribute and an optional
// pp_preshared_key extension request ("-" leaves either out).
const makeCSR = `require 'openssl'
key = OpenSSL::PKey::RSA.new(2048)
req = OpenSSL::X509::Request.new
req.version = 0
req.subject = OpenSSL::X509::Name.parse('/CN=' + ARGV[0])
req.public_key = key
if ARGV[1] != '-'
  req.add_attribute(OpenSSL::X509::Attribute.new('challengePassword',
    OpenSSL::ASN1::Set([OpenSSL::ASN1::UTF8String(ARGV[1])])))
end
if ARGV[2] != '-'
  ext = OpenSSL::X509::Extension.new('1.3.6.1.4.1.34380.1.1.4', OpenSSL::ASN1::UTF8String(ARGV[2]).to_der)
  req.add_attribute(OpenSSL::X509::Attribute.new('extReq', OpenSSL::ASN1::Set([OpenSSL::ASN1::Sequence([ext])])))
end
req.sign(key, OpenSSL::Digest.new('SHA256'))
print req.to_pem
`

func TestPSKAutosignScript(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not installed")
	}
	if err := exec.Command(ruby, "-ropenssl", "-e", "exit(OpenSSL.respond_to?(:secure_compare) ? 0 : 1)").Run(); err != nil {
		t.Skip("ruby openssl too old")
	}
	dir := t.TempDir()
	psk := `s3cr3t with spaces & "quotes" ;$(id)`
	if err := os.WriteFile(filepath.Join(dir, "psk"), []byte(psk+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(pskAutosignScript, pskMountPath+"/psk", filepath.Join(dir, "psk"), 1)
	if err := os.WriteFile(filepath.Join(dir, "autosign.rb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(dir, "gen.rb")
	if err := os.WriteFile(gen, []byte(makeCSR), 0o644); err != nil {
		t.Fatal(err)
	}
	csr := func(cn, challenge, ext string) []byte {
		out, err := exec.Command(ruby, gen, cn, challenge, ext).Output()
		if err != nil {
			t.Fatalf("generating CSR: %v", err)
		}
		return out
	}
	signs := func(certname string, pem []byte) bool {
		cmd := exec.Command(ruby, filepath.Join(dir, "autosign.rb"), certname)
		cmd.Stdin = bytes.NewReader(pem)
		return cmd.Run() == nil
	}

	for name, tc := range map[string]struct {
		certname, challenge, ext string
		corrupt, want            bool
	}{
		"challengePassword matches":    {"web01.example.com", psk, "-", false, true},
		"pp_preshared_key matches":     {"web01.example.com", "-", psk, false, true},
		"wrong challengePassword":      {"web01.example.com", "nope", "-", false, false},
		"wrong key, right other field": {"web01.example.com", "nope", psk, false, true},
		"no key at all":                {"web01.example.com", "-", "-", false, false},
		"prefix of the key":            {"web01.example.com", psk[:5], "-", false, false},
		"reserved service certname":    {"prod.ovx.svc", psk, "-", false, false},
		"garbage instead of a CSR":     {"web01.example.com", psk, "-", true, false},
	} {
		pem := csr(tc.certname, tc.challenge, tc.ext)
		if tc.corrupt {
			pem = []byte("-----BEGIN CERTIFICATE REQUEST-----\nnot base64\n-----END CERTIFICATE REQUEST-----\n")
		}
		if got := signs(tc.certname, pem); got != tc.want {
			t.Errorf("%s: signed=%v, want %v", name, got, tc.want)
		}
	}

	// No PSK configured: never sign.
	if err := os.WriteFile(filepath.Join(dir, "psk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if signs("web01.example.com", csr("web01.example.com", psk, "-")) {
		t.Error("an empty PSK file must deny everything")
	}
}

func TestHighSliceCardinality(t *testing.T) {
	for _, tc := range []struct {
		nodes, slices int32
		want          bool
	}{
		{nodes: 500, slices: 5, want: false},  // grouped by role: fine
		{nodes: 8, slices: 8, want: false},    // small fleet: not worth a warning
		{nodes: 300, slices: 290, want: true}, // one slice per machine
		{nodes: 300, slices: 40, want: false}, // 7.5 machines per slice
		{nodes: 30, slices: 16, want: true},   // under 2 per slice
	} {
		if got := highSliceCardinality(tc.nodes, tc.slices); got != tc.want {
			t.Errorf("%d nodes / %d slices: got %v, want %v", tc.nodes, tc.slices, got, tc.want)
		}
	}
	if msg := discoverySyncedMessage(300, 290, 0); !strings.Contains(msg, "WARNING") {
		t.Errorf("the Ready message must carry the warning, got %q", msg)
	}
}
