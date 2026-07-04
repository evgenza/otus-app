package security

import "testing"

func TestChecksum(t *testing.T) {
	sum := Checksum("привет")
	if len(sum) != 64 {
		t.Fatalf("ожидался hex-хеш длиной 64 символа, получено %d", len(sum))
	}
	if sum != Checksum("привет") {
		t.Error("хеш одинакового текста должен совпадать")
	}
	if sum == Checksum("привет!") {
		t.Error("хеш разного текста не должен совпадать")
	}
}
