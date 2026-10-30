package compose

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"orbsleep/internal/model"
)

const sharedFixture = `services:
  web:
    image: nginx:alpine
    environment:
      KEEP: web-value
    ports:
      - "8080:80"
      - "5353:5353/udp"
  api:
    image: alpine
    environment:
      KEEP: api-value
    ports:
      - "9000:9000"
`

func TestRestoreManagedBindingsPreservesOtherContent(t *testing.T) {
	current := []byte(`services:
  web:
    image: nginx:alpine
    environment:
      KEEP: web-value
    ports:
      - "5353:5353/udp"
  api:
    image: alpine
    environment:
      KEEP: api-edited
`)
	out, errs := RestoreManagedBindings(current, "web", []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}})
	if len(errs) > 0 {
		t.Fatalf("unexpected conflicts: %v", errs)
	}
	text := string(out)
	for _, want := range []string{"api-edited", "5353:5353/udp", "127.0.0.1:8080:80"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

func TestRestoreManagedBindingsRejectsOccupiedHostPort(t *testing.T) {
	current := []byte(`services:
  web:
    image: nginx:alpine
  api:
    image: alpine
    ports:
      - "8080:9000"
`)
	_, errs := RestoreManagedBindings(current, "web", []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}})
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "已被服务 api 占用") {
		t.Fatalf("expected host-port conflict, got %v", errs)
	}
}

func TestRestoreManagedBindingsIsIdempotent(t *testing.T) {
	current := []byte(`services:
  web:
    image: nginx:alpine
    ports:
      - "127.0.0.1:8080:80"
`)
	out, errs := RestoreManagedBindings(current, "web", []model.PortBinding{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}})
	if len(errs) > 0 {
		t.Fatalf("unexpected conflicts: %v", errs)
	}
	if string(out) != string(current) {
		t.Fatalf("idempotent restore changed the file:\n%s", out)
	}
}

func TestCaptureAndStoreSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, []byte(sharedFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := CaptureComposeSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.OriginalHash != snap.LastKnownGoodHash || !strings.Contains(string(snap.OriginalContent), "web") {
		t.Fatalf("bad snapshot: %+v", snap)
	}
	store := SnapshotStore{Root: filepath.Join(dir, "backups")}
	if _, err := store.Save("txn-1", *snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("txn-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.OriginalContent) != string(snap.OriginalContent) || string(loaded.LastKnownGood) != string(snap.LastKnownGood) {
		t.Fatal("loaded snapshot content differs")
	}
	// Corrupting the stored file must be detected on load.
	raw, _ := os.ReadFile(filepath.Join(store.Root, "txn-1", "lastgood.yml"))
	_ = os.WriteFile(filepath.Join(store.Root, "txn-1", "lastgood.yml"), append(raw, []byte("# tampered\n")...), 0o600)
	if _, err := store.Load("txn-1"); err == nil {
		t.Fatal("corrupt snapshot must be rejected")
	}
}

func TestFingerprintDetectsPermissionAndSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yml")
	link := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(target, []byte(sharedFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	fp1, err := FingerprintPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	fp2, err := FingerprintPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if fp1.SameFile(fp2) {
		t.Fatal("permission change must change the fingerprint")
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	fp3, err := FingerprintPath(link)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(target)
	if fp3.Kind != "symlink" || fp3.LinkTarget != resolved {
		t.Fatalf("symlink fingerprint = %+v", fp3)
	}
	if !fp3.SameContent(fp2) {
		t.Fatal("symlink target content must match")
	}
}

func TestSnapshotMetadataJSONRoundTrip(t *testing.T) {
	snap, err := CaptureComposeSnapshot(writeTempCompose(t, sharedFixture))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var back ComposeSnapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.OriginalHash != snap.OriginalHash || back.LastKnownGoodFingerprint.SHA256 == "" {
		t.Fatalf("roundtrip lost metadata: %+v", back)
	}
}

func writeTempCompose(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
