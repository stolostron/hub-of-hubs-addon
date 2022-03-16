package agent

import (
	"testing"
)

func TestCreateHohAgentManifestwork(t *testing.T) {
	agent, _ := CreateHohAgentManifestwork("test", "server", "ca")
	if agent.GetName() != "test-"+hohAgent {
		t.Fatalf("error")
	}
}
