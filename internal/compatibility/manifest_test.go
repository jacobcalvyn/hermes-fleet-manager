package compatibility

import "testing"

func TestCurrentManifestDefinesTheRuntimeContract(t *testing.T) {
	manifest := Current("1.2.3")
	if manifest.ControlPlaneVersion != "1.2.3" || manifest.HostAgentVersion != HostAgentVersion {
		t.Fatalf("manifest = %+v", manifest)
	}
	if !SupportsRuntimeSchema(RuntimeSchemaLegacy) || !SupportsRuntimeSchema(RuntimeSchemaCurrent) || SupportsRuntimeSchema(99) {
		t.Fatal("runtime schema compatibility is inconsistent")
	}
}
