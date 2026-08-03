package secrets

import "testing"

func TestIsSensitivePath(t *testing.T) {
	tests := []struct {
		name      string
		sensitive bool
	}{
		{name: ".env", sensitive: true},
		{name: ".env.local", sensitive: true},
		{name: ".env.example", sensitive: false},
		{name: ".env.template", sensitive: false},
		{name: "config/.aws/credentials", sensitive: true},
		{name: "home/.docker/config.json", sensitive: true},
		{name: "backup/.config/gh/hosts.yml", sensitive: true},
		{name: "home/.ssh/id_ed25519", sensitive: true},
		{name: ".ssh/id_ed25519", sensitive: true},
		{name: "keys/id_ed25519.pub", sensitive: false},
		{name: ".config/gh/hosts.yml", sensitive: true},
		{name: "certs/signing.pem", sensitive: true},
		{name: "certs/server-cert.pem", sensitive: false},
		{name: "certs/ca-key.pem", sensitive: true},
		{name: "certs/prod-cert-private-key.pem", sensitive: true},
		{name: "keys/server-key.pem", sensitive: true},
		{name: "internal/credentials.go", sensitive: false},
		{name: "docs/secrets.md", sensitive: false},
		{name: "config/example.yaml", sensitive: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsSensitivePath(test.name); got != test.sensitive {
				t.Fatalf("IsSensitivePath(%q) = %t, want %t", test.name, got, test.sensitive)
			}
		})
	}
}

func TestBinaryNames(t *testing.T) {
	if got := binaryNames("windows"); len(got) != 2 || got[0] != "trufflehog.exe" || got[1] != "trufflehog" {
		t.Fatalf("Windows binary names = %#v", got)
	}
	if got := binaryNames("darwin"); len(got) != 1 || got[0] != "trufflehog" {
		t.Fatalf("Unix binary names = %#v", got)
	}
}
