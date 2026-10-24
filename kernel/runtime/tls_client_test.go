package runtime

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run against the Deno on PATH, including the binary materialized by installation.
func TestDenoClientKeyFailureIsCatchable(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("TLS runtime regression requires Deno on PATH")
	}
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("explicit EC fixture generation requires OpenSSL")
	}
	directory := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Disposable runtime CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, kind string, der []byte) []byte {
		t.Helper()
		data := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		return data
	}
	caPEM := write("ca.crt", "CERTIFICATE", caDER)
	pair := func(name string, key crypto.Signer) tls.Certificate {
		t.Helper()
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(len(name) + 2)), Subject: pkix.Name{CommonName: name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, key.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		result, err := tls.X509KeyPair(write(name+".crt", "CERTIFICATE", der), write(name+".key", "PRIVATE KEY", private))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	namedKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate := pair("named", namedKey)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pair("rsa", rsaKey)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, openssl, "ec", "-in", filepath.Join(directory, "named.key"), "-param_enc", "explicit", "-out", filepath.Join(directory, "explicit.key"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("synthetic explicit key: %v: %s", err, output)
	}
	command = exec.CommandContext(ctx, openssl, "req", "-new", "-x509", "-key", filepath.Join(directory, "explicit.key"), "-subj", "/CN=synthetic-explicit", "-days", "1", "-out", filepath.Join(directory, "explicit.crt"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("synthetic explicit certificate: %v: %s", err, output)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Error("missing verified client certificate")
		}
		_, _ = w.Write([]byte("authenticated"))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCertificate}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	script := `
const [directory, endpoint] = Deno.args;
async function exercise({cert, key, caCerts, reject, endpoint}) {
  let client;
  try { client = Deno.createHttpClient({cert, key, caCerts}); }
  catch (error) {
    if (!reject || !(error instanceof TypeError)) throw error;
    return "rejected";
  }
  try {
    if (reject) throw new Error("unsupported key accepted");
    const response = await fetch(endpoint, {client});
    if (await response.text() !== "authenticated") throw new Error("client authentication failed");
    return "authenticated";
  } finally { client.close(); }
}
const caCerts = [await Deno.readTextFile(directory + "/ca.crt")];
for (const name of ["named", "rsa", "explicit"]) {
  const cert = await Deno.readTextFile(directory + "/" + name + ".crt");
  const key = await Deno.readTextFile(directory + "/" + name + ".key");
  const input = {cert, key, caCerts, endpoint, reject: name === "explicit"};
  console.log(name + " direct " + await exercise(input));
  const url = URL.createObjectURL(new Blob([exercise.toString() + ";onmessage = async e => postMessage(await exercise(e.data));"], {type: "application/javascript"}));
  const worker = new Worker(url, {type: "module", deno: {permissions: "inherit"}});
  try {
    const result = await new Promise((resolve, reject) => {
      worker.onmessage = event => resolve(event.data);
      worker.onerror = event => reject(new Error(event.message));
      worker.postMessage(input);
    });
    console.log(name + " worker " + result);
  } finally { worker.terminate(); URL.revokeObjectURL(url); }
}
console.log("parent survived");
`
	filename := filepath.Join(directory, "client.js")
	if err := os.WriteFile(filename, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	command = exec.CommandContext(ctx, deno, "run", "--no-config", "--unstable-worker-options", "--allow-read="+directory, "--allow-net="+server.Listener.Addr().String(), filename, directory, server.URL)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("client/Worker process failed: %v: %s", err, output)
	}
	for _, expected := range []string{"named direct authenticated", "named worker authenticated", "rsa direct authenticated", "rsa worker authenticated", "explicit direct rejected", "explicit worker rejected", "parent survived"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("missing %q: %s", expected, output)
		}
	}
	t.Log(strings.TrimSpace(string(output)))
}
