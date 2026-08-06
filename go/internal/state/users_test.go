package state

import "testing"

func TestUserRoundTrip(t *testing.T) {
	st := openTestStore(t)
	defer st.Close()

	if err := st.CreateUser(User{Username: "sanjin", Role: "operator", PasswordHash: "$argon2id$x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(User{Username: "sanjin", Role: "viewer", PasswordHash: "y"}); err != ErrUserExists {
		t.Fatalf("duplicate: %v", err)
	}
	if err := st.CreateUser(User{Username: "guest", Role: "viewer", PasswordHash: "z"}); err != nil {
		t.Fatal(err)
	}

	u, ok, err := st.UserByName("sanjin")
	if err != nil || !ok || u.Role != "operator" || u.PasswordHash != "$argon2id$x" {
		t.Fatalf("fetch: %v %v %+v", ok, err, u)
	}
	if _, ok, _ := st.UserByName("nobody"); ok {
		t.Fatal("phantom user")
	}

	users, err := st.ListUsers()
	if err != nil || len(users) != 2 {
		t.Fatalf("list: %v %d", err, len(users))
	}

	if n, _ := st.CountOperators(); n != 1 {
		t.Fatalf("operators: %d", n)
	}
	if err := st.SetUserDisabled("sanjin", true); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountOperators(); n != 0 {
		t.Fatalf("disabled operator still counted: %d", n)
	}

	if err := st.UpdateUserPassword("guest", "new-hash"); err != nil {
		t.Fatal(err)
	}
	u, _, _ = st.UserByName("guest")
	if u.PasswordHash != "new-hash" {
		t.Fatalf("password not updated: %s", u.PasswordHash)
	}

	if err := st.DeleteUser("guest"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser("guest"); err == nil {
		t.Fatal("double delete should error")
	}

	// A user row's JSON must never leak the hash.
	// (state.User marshals with json:"-" on PasswordHash.)
}

func TestUserRoleConstraint(t *testing.T) {
	st := openTestStore(t)
	defer st.Close()
	if err := st.CreateUser(User{Username: "x", Role: "admin", PasswordHash: "h"}); err == nil {
		t.Fatal("unknown role should violate the CHECK constraint")
	}
}
