package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCRDsOnlyAddRotationIntervalToV341(t *testing.T) {
	t.Parallel()

	const rotationProperty = "              rotationInterval:\n                type: string\n"
	tests := []struct {
		filename string
		baseline string
	}{
		{"secretgenerator.mittwald.de_basicauths_crd.yaml", "16be3509de5a80b15c63dad80cc96e6ebf935dd16accbd499591f047f1ed8870"},
		{"secretgenerator.mittwald.de_sshkeypairs_crd.yaml", "6c072fead6b5151e4151afd46839410ec9cd50a870c38d861f5a52ab08a8ddd6"},
		{"secretgenerator.mittwald.de_stringsecrets_crd.yaml", "58b0ec00d2802a180a39722e196aaf2accaaccbec7513b06cd2703786c2e8843"},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", "..", "deploy", "crds", tt.filename)
			crd, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := bytes.Count(crd, []byte(rotationProperty)); got != 1 {
				t.Fatalf("rotationInterval property count = %d, want 1", got)
			}

			withoutRotation := bytes.Replace(crd, []byte(rotationProperty), nil, 1)
			got := fmt.Sprintf("%x", sha256.Sum256(withoutRotation))
			if got != tt.baseline {
				t.Fatalf("CRD differs from v3.4.1 beyond rotationInterval: got sha256 %s, want %s", got, tt.baseline)
			}
		})
	}
}
