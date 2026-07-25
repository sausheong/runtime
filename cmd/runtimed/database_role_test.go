package main

import "testing"

func TestDatabaseRoleParsesURLAndKeywordDSNs(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		want string
	}{
		{"postgres://control:secret@db/runtime", "control"},
		{"host=db dbname=runtime user=agent password=secret", "agent"},
	} {
		got, err := databaseRole(tc.dsn)
		if err != nil {
			t.Fatalf("databaseRole(%q): %v", tc.dsn, err)
		}
		if got != tc.want {
			t.Fatalf("databaseRole(%q)=%q, want %q", tc.dsn, got, tc.want)
		}
	}
}

func TestDatabaseRoleDetectsEquivalentRolesAcrossDifferentDSNs(t *testing.T) {
	control, err := databaseRole("postgres://runtime:one@db/runtime")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := databaseRole("postgres://runtime:two@db/runtime?application_name=agent")
	if err != nil {
		t.Fatal(err)
	}
	if control != agent {
		t.Fatalf("effective roles differ: control=%q agent=%q", control, agent)
	}
}
