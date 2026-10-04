package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/task"
)

// A locked task records its module, so a resumed experiment grades in it whatever the project's setting is by then;
// a root task's lock, and so its digest, is what it was before modules (old locks resume, and still match their tasks).
func TestLockedTaskRecordsItsModule(t *testing.T) {
	spec := task.Spec{Base: "b", Solution: "s", HiddenTests: []string{"svc/a_test.go"}, Reference: []string{"svc/a.go"}, Setup: []string{"make"}, Verify: []string{"go test ./..."}}
	root := NewLockedTask("t", "Fix.", spec)
	// The shape of a lock made before modules.
	legacy := struct {
		Name        string   `json:"name"`
		Instruction string   `json:"instruction"`
		Base        string   `json:"base"`
		Solution    string   `json:"solution,omitempty"`
		HiddenTests []string `json:"hidden_tests,omitempty"`
		Reference   []string `json:"reference,omitempty"`
		Setup       []string `json:"setup,omitempty"`
		Verify      []string `json:"verify"`
		Digest      string   `json:"digest"`
	}{"t", "Fix.", "b", "s", spec.HiddenTests, spec.Reference, spec.Setup, spec.Verify, ""}
	encoded, _ := json.Marshal(legacy)
	sum := sha256.Sum256(encoded)
	if root.Digest != hex.EncodeToString(sum[:]) {
		t.Errorf("a root task's digest changed: %s", root.Digest)
	}
	if data, _ := json.Marshal(root); strings.Contains(string(data), "module") || root.Spec().Module != "" {
		t.Errorf("a root task's lock names a module: %s", data)
	}
	spec.Module = "services/billing"
	in := NewLockedTask("t", "Fix.", spec)
	if in.Digest == root.Digest || in.Module != "services/billing" || in.Spec().Module != "services/billing" {
		t.Errorf("a module task: %+v", in)
	}
	var back LockedTask
	data, _ := json.Marshal(in)
	if err := json.Unmarshal(data, &back); err != nil || back.Spec().Module != "services/billing" || back.Digest != in.Digest {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}
