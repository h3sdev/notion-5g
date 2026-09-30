package store

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// La migración de la sonda de celular corre sobre una COPIA de la base real
// de pruebas (data/live-test.db): nunca sobre el original, que está en git.
func TestMigrateLiveTestCopy(t *testing.T) {
	src := filepath.Join("..", "..", "data", "live-test.db")
	if _, err := os.Stat(src); err != nil {
		t.Skip("sin data/live-test.db")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "live-test.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(src+suffix, dst+suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ { // la segunda apertura comprueba que la migración es idempotente
		st, err := Open(dst)
		if err != nil {
			t.Fatalf("apertura %d: %v", i+1, err)
		}
		ctx := context.Background()
		if _, err := st.ListProbes(ctx, ""); err != nil {
			t.Fatalf("ListProbes: %v", err)
		}
		if _, err := st.ListCommands(ctx, "", "", 10); err != nil {
			t.Fatalf("ListCommands: %v", err)
		}
		if _, err := st.ListRaw(ctx, ListFilter{ProbeID: "hap-oficina"}); err != nil {
			t.Fatalf("ListRaw: %v", err)
		}
		if _, _, err := st.SweepPhoneOrders(ctx, "", time.Now()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if _, err := st.ProbeStatuses(ctx, time.Now()); err != nil {
			t.Fatalf("ProbeStatuses: %v", err)
		}
		if _, err := st.ListDeviceSummaries(ctx); err != nil {
			t.Fatalf("ListDeviceSummaries: %v", err)
		}
		st.Close()
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func TestOptASNUnmarshal(t *testing.T) {
	var p Probe
	if err := jsonUnmarshal(`{"targets":[{"device_id":"a","routing_table":"x"},{"device_id":"b","routing_table":"y","expected_asn":null},{"device_id":"c","routing_table":"z","expected_asn":3816}]}`, &p); err != nil {
		t.Fatal(err)
	}
	a, b, c := p.Targets[0].ExpectedASN, p.Targets[1].ExpectedASN, p.Targets[2].ExpectedASN
	if a.Set || !b.Set || b.Valid || !c.Valid || c.Value != 3816 {
		t.Fatalf("tres estados: %+v %+v %+v", a, b, c)
	}
	if err := jsonUnmarshal(`{"targets":[{"device_id":"a","routing_table":"x","expected_asn":"x"}]}`, &p); err == nil {
		t.Fatal("expected_asn texto debía fallar")
	}
}

func TestParseTSNormalizes(t *testing.T) {
	ts, err := ParseTS("2026-09-29T16:05:47.9-05:00")
	if err != nil || FormatTS(ts) != "2026-09-29T21:05:47Z" {
		t.Fatalf("%v %v", ts, err)
	}
	if id := NewUUID(); !ValidClientID(id) || len(id) != 36 || id[14] != '4' {
		t.Fatalf("uuid: %s", id)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
