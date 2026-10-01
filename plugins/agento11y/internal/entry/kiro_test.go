package entry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestKiroInstallAndUninstallDispatch(t *testing.T) {
	oldInstall, oldUninstall := kiroInstall, kiroUninstall
	t.Cleanup(func() { kiroInstall, kiroUninstall = oldInstall, oldUninstall })
	installed, removed := false, false
	kiroInstall = func() (bool, error) { installed = true; return true, nil }
	kiroUninstall = func() error { removed = true; return nil }
	var stdout, stderr bytes.Buffer
	withExit(t, func() { run([]string{"kiro", "install", "--json"}, strings.NewReader(""), &stdout, &stderr) })
	var receipt agentInstallResult
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if !installed || receipt.Agent != "kiro" || receipt.Status != "installed" {
		t.Fatalf("bad install receipt %+v", receipt)
	}
	withExit(t, func() { run([]string{"kiro", "uninstall"}, strings.NewReader(""), &stdout, &stderr) })
	if !removed {
		t.Fatal("uninstall not dispatched")
	}
	if stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
}
