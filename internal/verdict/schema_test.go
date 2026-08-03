package verdict

import "testing"

func TestIncludedPriorities(t *testing.T) {
	tests := []struct {
		maximum Priority
		want    string
	}{
		{PriorityP0, "P0"},
		{PriorityP1, "P0, P1"},
		{PriorityP2, "P0, P1, P2"},
		{PriorityP3, "P0, P1, P2, P3"},
	}
	for _, test := range tests {
		if got := IncludedPriorities(test.maximum); got != test.want {
			t.Fatalf("IncludedPriorities(%q) = %q, want %q", test.maximum, got, test.want)
		}
	}
}

func TestSchemaIsClosed(t *testing.T) {
	schema := Schema()
	if schema == "" || !contains(schema, `"additionalProperties": false`) {
		t.Fatalf("schema does not close objects: %s", schema)
	}
}

func contains(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
