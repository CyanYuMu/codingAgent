package memory

import "testing"

type fixedRecaller map[string][]Memory

func (f fixedRecaller) Recall(query string, topK int) ([]Memory, error) {
	got := f[query]
	if len(got) > topK {
		got = got[:topK]
	}
	return got, nil
}

func TestEvaluateRecallMetrics(t *testing.T) {
	r := fixedRecaller{
		"build": {
			{Key: "noise", Content: "unrelated result"},
			{Key: "build", Content: "构建命令是 make build"},
		},
		"language": {
			{Key: "language", Content: "用户偏好中文回复"},
			{Key: "duplicate", Content: "用户偏好中文回复 "},
		},
	}
	got, err := EvaluateRecall(r, []RecallCase{
		{Query: "build", RelevantKeys: []string{"build"}},
		{Query: "language", RelevantKeys: []string{"language"}},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cases != 2 || got.HitRateAtK != 1 || got.RecallAtK != 1 || got.MRR != 0.75 {
		t.Fatalf("metrics = %+v", got)
	}
	if got.DuplicateRate <= 0 {
		t.Fatalf("near duplicate should be visible in metrics: %+v", got)
	}
}

func TestEvaluateRecallProductionUnionHasNoDuplicatePressure(t *testing.T) {
	dir := t.TempDir()
	proj, _ := Open(dir+"/project.db", ScopeProject, "p")
	defer proj.Close()
	global, _ := Open(dir+"/global.db", ScopeGlobal, "")
	defer global.Close()
	remember(t, proj, "用户明确偏好所有技术讨论都使用简体中文回复", Opts{Key: "language"})
	remember(t, global, "用户明确偏好所有技术讨论都使用简体中文回复。", Opts{Key: "language-copy"})
	got, err := EvaluateRecall(Union(proj, global), []RecallCase{{
		Query: "技术讨论语言偏好", RelevantKeys: []string{"language"},
	}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.HitRateAtK != 1 || got.DuplicateRate != 0 {
		t.Fatalf("production union metrics = %+v", got)
	}
}
