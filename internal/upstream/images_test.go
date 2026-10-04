package upstream

import "testing"

// TestNonChatModelAndGenerationKind 非对话模型判定必须覆盖媒体生成标签：
// 出图（text-to-image / image-to-image）与出视频（text-to-video / image-to-video）。
// 视频这两条以前漏了 —— seedance-2.5 因此被当成对话模型列进 global 名单，面板里还
// 显示成"不支持思考"；客户端选它发对话只会拿到 11102/14407。
func TestNonChatModelAndGenerationKind(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		maxOut   int64
		tags     []string
		nonChat  bool
		kindWant string
	}{
		{"出图", "hunyuan-image-alpha", 0, []string{"text-to-image"}, true, "image"},
		{"改图", "hunyuan-image-alpha-edit", 0, []string{"image-to-image"}, true, "image"},
		{"出视频", "seedance-2.5", 0, []string{"text-to-video", "image-to-video"}, true, "video"},
		{"改视频（大写/空白容错）", "x", 0, []string{" Text-To-Video "}, true, "video"},
		{"普通对话模型", "glm-5.2", 32000, []string{"craft"}, false, ""},
		{"craft + 视频标签仍算出视频", "y", 32000, []string{"craft", "text-to-video"}, true, "video"},
		{"前缀过滤仍生效", "codewise-jump", 0, nil, true, ""},
		{"tiny 输出仍过滤", "z", 256, nil, true, ""},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.maxOut, c.tags); got != c.nonChat {
			t.Errorf("%s: nonChatModel=%v want %v", c.name, got, c.nonChat)
		}
		if got := generationKind(c.tags); got != c.kindWant {
			t.Errorf("%s: generationKind=%q want %q", c.name, got, c.kindWant)
		}
	}
}

// TestStashGenerationModelsMergesPerID 留存按 ID 合并而非覆盖：global 目录是多 UA 并发
// 探测的，宽/窄两份目录都会落一笔——覆盖式留存会让结果取决于谁先返回（实测表现为
// 「这次有 gpt-image-2.5-sunburst、下次变成 hunyuan-image-alpha」，seedance-2.5 时有时无）。
func TestStashGenerationModelsMergesPerID(t *testing.T) {
	c := &Client{}
	c.stashGenerationModels("global", []ModelInfo{
		{ID: "gpt-image-2.5-sunburst", Tags: []string{"text-to-image"}},
	})
	c.stashGenerationModels("global", []ModelInfo{
		{ID: "hunyuan-image-alpha", Tags: []string{"text-to-image"}},
		{ID: "seedance-2.5", Tags: []string{"text-to-video", "image-to-video"}},
		{ID: "glm-5.2", Tags: []string{"craft"}}, // 非媒体模型不入留存
	})
	got := c.GenerationModels("global")
	if len(got) != 3 {
		t.Fatalf("留存应合并成 3 条（窄目录不能顶掉宽目录），实际 %d：%+v", len(got), got)
	}
	kinds := map[string]string{}
	for _, m := range got {
		kinds[m.ID] = m.GenerationKind
	}
	if kinds["gpt-image-2.5-sunburst"] != "image" || kinds["hunyuan-image-alpha"] != "image" ||
		kinds["seedance-2.5"] != "video" {
		t.Fatalf("kind 标记不对：%+v", kinds)
	}
	// 顺序稳定（按 ID 排序），重复留存不抖
	again := c.GenerationModels("global")
	for i := range got {
		if got[i].ID != again[i].ID {
			t.Fatalf("留存顺序不稳定：%v vs %v", got, again)
		}
	}
}
