package dbtest

import "testing"

func TestCheck(t *testing.T) {
	ok := []string{
		"postgres://sajni_test:x@localhost:55432/sajni_test?sslmode=disable",
		"postgresql://u@127.0.0.1/my_test_db",
	}
	bad := []string{
		"postgres://u:p@db.abcd.supabase.co:5432/postgres",
		"postgres://u:p@localhost/postgres",
		"host=localhost dbname=sajni_test",
	}
	for _, s := range ok {
		if why := Check(s); why != "" {
			t.Errorf("%s refused: %s", s, why)
		}
	}
	for _, s := range bad {
		if Check(s) == "" {
			t.Errorf("%s allowed", s)
		}
	}
}
