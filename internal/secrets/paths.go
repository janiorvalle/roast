package secrets

import (
	"path"
	"strings"
)

// IsSensitivePath reports whether a repository path is a known environment or
// credential store. These files stay local to the user's machine and never
// enter a review bundle.
func IsSensitivePath(name string) bool {
	name = path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		return true
	}

	base := strings.ToLower(path.Base(name))
	if strings.HasPrefix(base, ".env") && !isEnvironmentTemplate(base) {
		return true
	}
	if isSensitiveStore(name, base) {
		return true
	}
	if (strings.HasPrefix(base, "id_rsa") || strings.HasPrefix(base, "id_ed25519") || strings.HasPrefix(base, "id_ecdsa") || strings.HasPrefix(base, "id_dsa")) && !strings.HasSuffix(base, ".pub") {
		return true
	}

	switch strings.ToLower(path.Ext(base)) {
	case ".pem":
		return !isPublicPEMName(base)
	case ".key", ".p12", ".pfx", ".jks":
		return true
	default:
		return false
	}
}

func isSensitiveStore(name, base string) bool {
	lowerName := strings.ToLower(name)
	for _, store := range []string{
		".aws/credentials",
		".aws/config",
		".azure/accessTokens.json",
		".azure/azureProfile.json",
		".config/gcloud/application_default_credentials.json",
		".config/gcloud/credentials.db",
		".config/gh/hosts.yml",
		".docker/config.json",
		".kube/config",
		".git-credentials",
		".netrc",
		".authinfo",
		".npmrc",
		".pypirc",
	} {
		lowerStore := strings.ToLower(store)
		if lowerName == lowerStore || strings.HasSuffix(lowerName, "/"+lowerStore) {
			return true
		}
	}

	if hasPathComponent(name, ".ssh") {
		return true
	}
	if hasSensitiveStoreExtension(base, "credentials.") || hasSensitiveStoreExtension(base, "secrets.") || hasSensitiveStoreExtension(base, "service-account") {
		return true
	}
	switch base {
	case "credentials", "secrets", "credentials.json", "credentials.yml", "credentials.yaml", "secrets.json", "secrets.yml", "secrets.yaml":
		return true
	default:
		return false
	}
}

func isEnvironmentTemplate(base string) bool {
	for _, suffix := range []string{".defaults", ".dist", ".example", ".sample", ".template"} {
		if strings.HasPrefix(base, ".env"+suffix) {
			return true
		}
	}
	return false
}

func isPublicPEMName(base string) bool {
	stem := strings.TrimSuffix(base, ".pem")
	parts := strings.FieldsFunc(stem, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for _, part := range parts {
		if part == "key" || part == "private" || part == "secret" {
			return false
		}
	}
	for _, part := range parts {
		switch part {
		case "ca", "cert", "certificate", "certificates", "chain", "pub", "public":
			return true
		}
	}
	return false
}

func hasPathComponent(name, component string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, component) {
			return true
		}
	}
	return false
}

func hasSensitiveStoreExtension(base, prefix string) bool {
	if !strings.HasPrefix(base, prefix) {
		return false
	}
	switch path.Ext(base) {
	case ".json", ".yml", ".yaml", ".toml", ".ini", ".cfg", ".conf":
		return true
	default:
		return false
	}
}
