package main

import "testing"

// 登录名中的 @ 决定认证方式，后缀内容不影响路由。
func TestUsesLDAPLogin(t *testing.T) {
	for _, test := range []struct {
		username string
		wantLDAP bool
	}{
		{username: "Admin", wantLDAP: false},
		{username: "reader", wantLDAP: false},
		{username: "reader@example.org", wantLDAP: true},
		{username: "reader@local", wantLDAP: true},
	} {
		if got := usesLDAPLogin(test.username); got != test.wantLDAP {
			t.Errorf("usesLDAPLogin(%q) = %t, want %t", test.username, got, test.wantLDAP)
		}
	}
}
