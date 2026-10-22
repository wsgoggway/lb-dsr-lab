package health

import "testing"

func TestCombine(t *testing.T) {
	tests := []struct {
		name    string
		local   string
		peer    string
		mode    string
		want    string
		wantErr bool
	}{
		{"local mode ignores peer", StateUP, StateDOWN, "local", StateUP, false},
		{"local mode keeps local down", StateDOWN, StateUP, "local", StateDOWN, false},
		{"and both up", StateUP, StateUP, "and", StateUP, false},
		{"and peer down", StateUP, StateDOWN, "and", StateDOWN, false},
		{"and local down", StateDOWN, StateUP, "and", StateDOWN, false},
		{"and inactive counts as down", StateUP, StateUnknown, "and", StateDOWN, false},
		{"or any up", StateDOWN, StateUP, "or", StateUP, false},
		{"or both down", StateDOWN, StateDOWN, "or", StateDOWN, false},
		{"or local up", StateUP, StateDOWN, "or", StateUP, false},
		{"unknown mode errors", StateUP, StateUP, "quorum", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Combine(tt.local, tt.peer, tt.mode)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Combine(%q,%q,%q) error = nil, want error", tt.local, tt.peer, tt.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Combine(%q,%q,%q) error = %v", tt.local, tt.peer, tt.mode, err)
			}
			if got != tt.want {
				t.Fatalf("Combine(%q,%q,%q) = %q, want %q", tt.local, tt.peer, tt.mode, got, tt.want)
			}
		})
	}
}
