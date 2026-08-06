package control

import "testing"

func TestDefaultCommandCapFallsBackToConstant(t *testing.T) {
	s := &State{}
	if got := s.defaultCommandCap(); got != MaxCommandW {
		t.Errorf("zero DefaultCommandW: got %v, want %v", got, float64(MaxCommandW))
	}
	s.DefaultCommandW = 25000
	if got := s.defaultCommandCap(); got != 25000 {
		t.Errorf("got %v, want 25000", got)
	}
}

func TestBatteryCapsPreferDriverThenSiteThenConstant(t *testing.T) {
	b := batteryInfo{}
	if b.chargeCap() != MaxCommandW || b.dischargeCap() != MaxCommandW {
		t.Errorf("bare batteryInfo should fall back to MaxCommandW, got %v/%v", b.chargeCap(), b.dischargeCap())
	}
	b.defaultCapW = 20000
	if b.chargeCap() != 20000 || b.dischargeCap() != 20000 {
		t.Errorf("site default should win over constant, got %v/%v", b.chargeCap(), b.dischargeCap())
	}
	b.maxChargeW = 8000
	b.maxDischargeW = 6000
	if b.chargeCap() != 8000 || b.dischargeCap() != 6000 {
		t.Errorf("driver limits should win over site default, got %v/%v", b.chargeCap(), b.dischargeCap())
	}
}
