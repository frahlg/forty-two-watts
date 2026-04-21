package state

import (
	"path/filepath"
	"testing"
)

func TestNovaDER_UpsertGetList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.UpsertNovaDER(NovaDER{
		DeviceID: "ferroamp:ES9234", DerType: "battery",
		DerName: "ferroamp-battery", DerID: "der-abc",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNovaDER(NovaDER{
		DeviceID: "ferroamp:ES9234", DerType: "meter",
		DerName: "ferroamp-meter", DerID: "der-def",
	}); err != nil {
		t.Fatal(err)
	}
	// Upsert: same key, new der_id.
	if err := st.UpsertNovaDER(NovaDER{
		DeviceID: "ferroamp:ES9234", DerType: "battery",
		DerName: "ferroamp-battery", DerID: "der-xyz",
	}); err != nil {
		t.Fatal(err)
	}

	got := st.GetNovaDER("ferroamp:ES9234", "battery")
	if got == nil || got.DerID != "der-xyz" {
		t.Fatalf("upsert did not overwrite: %+v", got)
	}
	if st.GetNovaDER("nope", "battery") != nil {
		t.Fatal("missing row should return nil")
	}

	list, err := st.ListNovaDERs()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v count=%d", err, len(list))
	}

	if err := st.DeleteNovaDER("ferroamp:ES9234", "meter"); err != nil {
		t.Fatal(err)
	}
	if st.GetNovaDER("ferroamp:ES9234", "meter") != nil {
		t.Fatal("delete did not remove row")
	}
}

func TestNovaDER_RejectsEmpty(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.db"))
	defer st.Close()
	if err := st.UpsertNovaDER(NovaDER{DeviceID: "", DerType: "battery"}); err == nil {
		t.Fatal("empty device_id should error")
	}
}
