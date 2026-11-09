package config

import "testing"

func TestParseDotEnv(t *testing.T) {
	got, err := parseDotEnv("\ufeff# comment\r\nexport APP_PASSWORD = \"a\\n b\"\r\nNAME='raw # value'\r\nPORT=7080 # note\r\nEMPTY=\r\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"APP_PASSWORD": "a\n b", "NAME": "raw # value", "PORT": "7080", "EMPTY": ""}
	for _, entry := range got {
		if want[entry.Key] != entry.Value {
			t.Errorf("%s=%q want %q", entry.Key, entry.Value, want[entry.Key])
		}
		delete(want, entry.Key)
	}
	if len(want) != 0 {
		t.Fatalf("missing entries %v", want)
	}
}

func TestParseDotEnvRejectsMalformedLine(t *testing.T) {
	for _, input := range []string{"=bad", "BAD-KEY=x", "A=\"unclosed"} {
		if _, err := parseDotEnv(input); err == nil {
			t.Errorf("expected parse error for %q", input)
		}
	}
}
