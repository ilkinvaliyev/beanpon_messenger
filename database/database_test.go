package database

import (
	"errors"
	"testing"
)

// PgBouncer'ın reddi TANINMALI — tanınmazsa yedek yol devreye girmez ve
// servis ayağa kalkmaz. (Canlıda yaşandı.)
func TestIsUnsupportedStartupParam(t *testing.T) {
	real := errors.New(
		"failed to connect to `user=beanpon_user database=beanpon_new_db`: " +
			"172.18.0.6:5432 (beanponapp_pgbouncer): server error: " +
			"FATAL: unsupported startup parameter: statement_timeout (SQLSTATE 08P01)")
	if !isUnsupportedStartupParam(real) {
		t.Fatal("canlıdaki gerçek hata tanınmadı — yedek yol çalışmaz")
	}

	for _, e := range []error{
		errors.New("FATAL: unsupported startup parameter: extra_float_digits"),
		errors.New("server error: SQLSTATE 08P01 startup packet"),
	} {
		if !isUnsupportedStartupParam(e) {
			t.Fatalf("tanınmadı: %v", e)
		}
	}

	// BAŞKA hatalar yedek yolu tetiklememeli — yanlış parola ile sonsuz
	// yeniden deneme olmasın.
	for _, e := range []error{
		nil,
		errors.New("password authentication failed for user"),
		errors.New("dial tcp: connection refused"),
		errors.New("database does not exist"),
	} {
		if isUnsupportedStartupParam(e) {
			t.Fatalf("yanlış tanındı: %v", e)
		}
	}
}
