package blocks

import "testing"

// TestDowngradeOccludes pins ISO_VOXEL_PLAN.md §5 Phase 4's one-directional
// contract: it can only clear an occluding block's flag (real texture alpha
// disagreeing with blocks.json), never set a non-occluding block's flag
// true -- putLocked's derivation from Transparent/Water/Decoration remains
// the sole source of truth for why a block occludes at all.
func TestDowngradeOccludes(t *testing.T) {
	r := NewRegistry()
	n, err := r.LoadBlocksJSON([]byte(`{
		"test:leaf": {"color": "#00ff00", "transparent": false},
		"test:air_like": {"color": "#000000", "transparent": true}
	}`))
	if err != nil || n != 2 {
		t.Fatalf("LoadBlocksJSON: n=%d err=%v", n, err)
	}

	leafID := r.ID("test:leaf")
	if !r.Get(leafID).Occludes {
		t.Fatal("precondition: opaque block should start Occludes=true")
	}
	r.DowngradeOccludes(leafID)
	if r.Get(leafID).Occludes {
		t.Error("DowngradeOccludes did not clear Occludes")
	}

	airLikeID := r.ID("test:air_like")
	if r.Get(airLikeID).Occludes {
		t.Fatal("precondition: transparent block should start Occludes=false")
	}
	r.DowngradeOccludes(airLikeID) // must stay false; there is nothing to downgrade
	if r.Get(airLikeID).Occludes {
		t.Error("DowngradeOccludes somehow set Occludes=true")
	}

	// Out-of-range id must be a silent no-op, not a panic.
	r.DowngradeOccludes(65000)
}
