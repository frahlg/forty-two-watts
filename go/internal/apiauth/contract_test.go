package apiauth

import (
	"os"
	"testing"

	"github.com/srcfl/ftw/go/internal/appproto/gencontract"
)

// The role table must be what the registry currently says. A snapshot updated
// without rerunning the generator is exactly the drift the registry exists to
// prevent — and a role is the one name here that decides whether a phone may
// change anything.
func TestRoleTableIsCurrent(t *testing.T) {
	raw, err := os.ReadFile("../../../contract/registry.yaml")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	want, err := gencontract.GenerateRoles(raw)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile("contract_gen.go")
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("apiauth/contract_gen.go is stale; run: go generate ./internal/...")
	}
}

// Every role must carry at least one scope. A role that carries none is one
// nobody can use and one no refusal can explain.
func TestEveryRoleCarriesSomething(t *testing.T) {
	if len(RoleScopes) == 0 {
		t.Fatal("the registry defines no roles")
	}
	for role, scopes := range RoleScopes {
		if len(scopes) == 0 {
			t.Fatalf("role %q carries no scopes", role)
		}
	}
	if _, ok := RoleScopes[RoleOwner]; !ok {
		t.Fatal("there is no owner role; a box with no owner cannot be administered")
	}
}

// The zero Caller can do nothing. Anything else would make a Caller nobody
// filled in more powerful than a viewer.
func TestTheZeroCallerCarriesNoAuthority(t *testing.T) {
	var nobody Caller
	for _, scope := range RoleScopes[RoleOwner] {
		if nobody.Scopes.Has(scope) {
			t.Fatalf("an empty caller holds %q", scope)
		}
	}
	if nobody.Role != "" || nobody.StepUp {
		t.Fatalf("the zero caller is %+v", nobody)
	}
}
