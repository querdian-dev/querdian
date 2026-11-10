package main

import "testing"

func TestGoMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		env     map[string]string
		want    int64
		wantErr bool
	}{
		{env: nil, want: 0},
		{env: map[string]string{memoryLimitEnv: "536870912"}, want: 536870912 / 100 * 90},
		{env: map[string]string{memoryLimitEnv: "536870912", "GOMEMLIMIT": "100MiB"}, want: 0},
		{env: map[string]string{memoryLimitEnv: "512Mi"}, wantErr: true},
		{env: map[string]string{memoryLimitEnv: "0"}, wantErr: true},
	} {
		got, err := goMemoryLimit(func(k string) string { return tc.env[k] })
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%v: got %d, %v; want %d, error %v", tc.env, got, err, tc.want, tc.wantErr)
		}
	}
	if err := applyMemoryLimit(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
}
