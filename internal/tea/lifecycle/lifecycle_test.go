package lifecycle

import (
	"fmt"
	"reflect"
	"testing"
)

// TestBoundedModelChecking exhaustively explores reachable states up to depth 5
// over a symbolic alphabet of Msg inputs and checks:
//  1. Base Invariant: Invariant(Init("dev")) == nil.
//  2. Immutability: Update(m, msg) never mutates m in place.
//  3. Determinism: calling Update(m, msg) twice yields identical (Model, []Effect).
//  4. Inductive State Invariant: Invariant(m) == nil => Invariant(next) == nil.
//  5. Transition Relation Invariant (IronFleet / TLA+ Next): StepValid(m, msg, next, eff) == nil.
func TestBoundedModelChecking(t *testing.T) {
	initial := Init("dev")
	if err := Invariant(initial); err != nil {
		t.Fatalf("initial model violates invariant: %v", err)
	}

	alphabet := []Msg{
		MsgApplyBuilt{System: "/nix/store/sys-a", KernelHash: "kern-1"},
		MsgApplyBuilt{System: "/nix/store/sys-b", KernelHash: "kern-1"},
		MsgApplyBuilt{System: "/nix/store/sys-c", KernelHash: "kern-2"},
		MsgStart{KernelHash: "kern-1"},
		MsgSwitchSucceeded{KernelHash: "kern-1"},
		MsgSwitchSucceeded{KernelHash: "kern-2"},
		MsgSwitchFailed{Reason: "activation failed"},
		MsgStop{},
		MsgMountShare{Path: "/home/user/proj-a", Tag: "tag-a", ReadOnly: false},
		MsgMountShare{Path: "/home/user/proj-b", Tag: "tag-b", ReadOnly: true},
		MsgUnmountShare{Path: "/home/user/proj-a"},
		MsgUnmountShare{Path: "/home/user/proj-b"},
	}

	visited := map[string]bool{}
	frontier := []Model{initial}
	visited[stateKey(initial)] = true

	const maxDepth = 5
	for depth := 0; depth < maxDepth; depth++ {
		var nextFrontier []Model
		for _, m := range frontier {
			for _, msg := range alphabet {
				snapshot := cloneModel(m)
				m1, eff1 := Update(m, msg)
				m2, eff2 := Update(m, msg)

				// 1. Immutability check
				if !reflect.DeepEqual(m, snapshot) {
					t.Fatalf("Update mutated input Model on %T: before=%+v after=%+v", msg, snapshot, m)
				}
				// 2. Determinism check
				if !reflect.DeepEqual(m1, m2) || !reflect.DeepEqual(eff1, eff2) {
					t.Fatalf("non-deterministic Update on %T from %+v", msg, m)
				}
				// 3. Inductive state invariant check
				if err := Invariant(m1); err != nil {
					t.Fatalf("invariant violated at depth %d after %#v on state %+v: %v", depth+1, msg, m, err)
				}
				// 4. Transition relation invariant check (IronFleet / TLA+)
				if err := StepValid(m, msg, m1, eff1); err != nil {
					t.Fatalf("step relation violated at depth %d after %#v on state %+v -> %+v: %v",
						depth+1, msg, m, m1, err)
				}

				k := stateKey(m1)
				if !visited[k] {
					visited[k] = true
					nextFrontier = append(nextFrontier, m1)
				}
			}
		}
		frontier = nextFrontier
	}

	if len(visited) < 20 {
		t.Fatalf("expected state space exploration to reach >= 20 states, got %d", len(visited))
	}
}

func stateKey(m Model) string {
	return fmt.Sprintf("%+v", m)
}
