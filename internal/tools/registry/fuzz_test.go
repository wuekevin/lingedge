package registry

import (
	"bytes"
	"testing"
)

// FuzzParseJournal：任意字节都不得 panic；成功时序列号严格连续、首条是 open，且步骤数不超过 begin 数。
func FuzzParseJournal(f *testing.F) {
	f.Add([]byte(`{"v":1,"seq":1,"ts":"2026-11-21T12:00:00.000Z","op":"open","tx":"01JD5Y3C8Q5G7M0V1N2P3R4S5T","components":["server"]}` + "\n" +
		`{"v":1,"seq":2,"ts":"2026-11-21T12:00:00.000Z","op":"begin","step":1,"name":"a","undo":{"actions":[{"action":"remove_path","path":"/x","created":true}]}}` + "\n" +
		`{"v":1,"seq":3,"ts":"2026-11-21T12:00:00.000Z","op":"done","step":1}` + "\n" +
		`{"v":1,"seq":4,"ts":"2026-11-21T12:00:00.000Z","op":"commit"}` + "\n"))
	f.Add([]byte(`{"v":1,"seq":1,"op":"open","tx":"01JD5Y3C8Q5G7M0V1N2P3R4S5T"}` + "\n" + `{"v":1,"seq":2,"op":"be`))
	f.Add([]byte("\x00\x00\n{}\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		tx, err := ParseJournal(bytes.NewReader(data))
		if tx == nil {
			t.Fatal("ParseJournal 不应返回 nil")
		}
		for i, rec := range tx.Records {
			if rec.Seq != i+1 {
				t.Fatalf("seq 不连续: %d@%d", rec.Seq, i)
			}
		}
		if len(tx.Records) > 0 && tx.Records[0].Op != OpOpen {
			t.Fatal("首条必须是 open")
		}
		if err == nil {
			begun := 0
			for _, rec := range tx.Records {
				if rec.Op == OpBegin {
					begun++
				}
			}
			if len(tx.Steps) > begun {
				t.Fatalf("步骤数 %d > begin 数 %d", len(tx.Steps), begun)
			}
			_ = tx.UndoPlan()
			_ = tx.ResumeFrom()
			_ = tx.InFlight()
			if tx.TornTail && tx.TornOffset > int64(len(data)) {
				t.Fatal("TornOffset 越界")
			}
		}
	})
}

// FuzzParseState：任意字节都不得 panic；成功解析的状态必须能再序列化、再解析且等价（往返稳定）。
func FuzzParseState(f *testing.F) {
	f.Add([]byte(`{"schema_version":1,"components":{"server":{"version":"1.2.3","state":"running","unit":"lingedge-server.service","run_user":"lingedge","ports":[8080],"release_dir":"/opt/lingedge/server/releases/1.2.3","config_dir":"","data_dir":"","installed_at":"2026-11-21T12:00:00.000Z"}}}`))
	f.Add([]byte(`{"schema_version":2}`))
	f.Add([]byte(`{"schema_version":1,"components":{}}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParseState(data)
		if err != nil {
			return
		}
		b, err := MarshalState(s)
		if err != nil {
			t.Fatalf("已通过解析的状态应可序列化: %v", err)
		}
		s2, err := ParseState(b)
		if err != nil {
			t.Fatalf("往返后应可解析: %v", err)
		}
		b2, _ := MarshalState(s2)
		if !bytes.Equal(b, b2) {
			t.Fatal("往返不稳定")
		}
	})
}
