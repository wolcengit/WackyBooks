package main

// 本文件验证首次建库、整数主键、外键完整性和重复打开数据库。
import "testing"

func TestInitializeStorage(t *testing.T) {
	dir := t.TempDir()
	db, err := openStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"settings", "libraries", "users", "readers", "books", "grants", "reading", "lists", "favorites"} {
		var keys, integerIDs int
		err := db.QueryRow(`SELECT count(*),count(*) FILTER (WHERE name='id' AND type='INTEGER') FROM pragma_table_info(?) WHERE pk>0`, table).Scan(&keys, &integerIDs)
		if err != nil || keys != 1 || integerIDs != 1 {
			t.Errorf("%s: expected a single integer id primary key, got %d keys and %d integer ids: %v", table, keys, integerIDs, err)
		}
	}
	var oidc string
	if err := db.QueryRow(`SELECT oidc FROM settings WHERE id=1`).Scan(&oidc); err != nil || oidc == "" {
		t.Fatalf("OIDC settings were not initialized: value=%q error=%v", oidc, err)
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Errorf("foreign key violations: %d, error: %v", violations, err)
	}
	if _, err := db.Exec(`UPDATE settings SET password='test-hash' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := loadSettings(db)
	if err != nil || state.Password != "test-hash" {
		t.Fatalf("existing settings were not preserved: %v", err)
	}
}
