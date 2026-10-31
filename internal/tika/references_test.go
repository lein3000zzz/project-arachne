package tika

import (
	"strings"
	"testing"
	"web-crawler/internal/domain/data"
)

func TestReferencesOfAPaper(t *testing.T) {
	// As Tika gives them, after clean: one paragraph per entry of the list.
	text := "Code is at [https://github.com/huggingface/rag/](https://github.com/huggingface/rag/).\n\n" +
		"[1] Payal Bajaj and others. MS MARCO: A Human Generated MAchine Reading COmprehension Dataset. " +
		"arXiv:1611.09268 [cs], November 2016. URL http: //arxiv.org/abs/1611.09268. arXiv: 1611.09268.\n\n" +
		"[5] Danqi Chen, Adam Fisch. Reading Wikipedia to Answer Open-Domain Questions. In ACL, 2017. " +
		"doi: 10.18653/v1/P17-1171. URL https://www.aclweb.org/anthology/P17-1171.\n\n" +
		"[9] A. Author. A paper with a doi.org link. URL https://doi.org/10.1145/3292500.3330701.\n\n" +
		"[24] C.-Y. Lee, S. Xie. Deeply- supervised nets. arXiv:1409.5185v2, 2014.\n\n" +
		"1http://image-net.org/challenges/LSVRC/2015/ and http://host.robots.ox.ac.uk:8080/displaylb.php?challengeid=11\\&compid=4.\n\n" +
		// Broken at line ends by the PDF; a word after a link stays a word.
		"[13] Angela Fan. Augmenting transformers, 2020. URL https://openreview.net/forum?id= H1gx1CNKPH.\n\n" +
		"[14] B. Author. In EMNLP, 2019. doi: 10.18653/v1/ D19-1250. URL https://www.aaai.org/ocs/index. php/AAAI/paper/view/16567.\n\n" +
		"[15] C. Author. URL https://example.org/c.pdf. 2019.\n\n" +
		"Found at https://github.com/huggingface/transformers/blob/master/ examples/rag/. Or https://huggingface.co/ and more.\n\n" +
		"[https://github.com/huggingface/transformers/blob/master/examples/rag/](https://github.com/huggingface/transformers/blob/master/examples/rag/)"

	want := []string{
		"https://github.com/huggingface/rag/",
		"https://arxiv.org/abs/1611.09268",
		"https://doi.org/10.18653/v1/P17-1171",
		"https://www.aclweb.org/anthology/P17-1171",
		"https://doi.org/10.1145/3292500.3330701",
		"https://arxiv.org/abs/1409.5185",
		"http://image-net.org/challenges/LSVRC/2015/",
		"http://host.robots.ox.ac.uk:8080/displaylb.php?challengeid=11&compid=4",
		"https://openreview.net/forum?id=H1gx1CNKPH",
		"https://doi.org/10.18653/v1/D19-1250",
		"https://www.aaai.org/ocs/index.php/AAAI/paper/view/16567",
		"https://example.org/c.pdf",
		"https://github.com/huggingface/transformers/blob/master/examples/rag/",
		"https://huggingface.co/",
	}

	got := references(text)
	if len(got) != len(want) {
		t.Fatalf("got %d references, want %d: %+v", len(got), len(want), got)
	}

	for i, ref := range got {
		if ref.URL != want[i] {
			t.Errorf("reference %d = %q, want %q", i, ref.URL, want[i])
		}
	}

	if got[0].Label != "Code is at https://github.com/huggingface/rag/." {
		t.Errorf("a link's label shows its text: %q", got[0].Label)
	}

	if got[5].Label != "[24] C.-Y. Lee, S. Xie. Deeply- supervised nets. arXiv:1409.5185v2, 2014." {
		t.Errorf("a short entry is its own label: %q", got[5].Label)
	}

	if got[6].Label != "1http://image-net.org/challenges/LSVRC/2015/ and http://host.robots.ox.ac.uk:8080/displaylb.php?challengeid=11&compid=4." {
		t.Errorf("no space put where there was none, no LaTeX escapes: %q", got[6].Label)
	}

	if !strings.HasPrefix(got[12].Label, "Found at ") {
		t.Errorf("the citing sentence is the label, not a later bare link: %q", got[12].Label)
	}
}

func TestALongReferenceEntryIsLabelledByItsStart(t *testing.T) {
	text := "[5] Danqi Chen, Adam Fisch, Jason Weston, and Antoine Bordes. Reading Wikipedia to Answer Open-Domain Questions. " +
		"In Proceedings of the 55th Annual Meeting of the Association for Computational Linguistics (Volume 1: Long Papers), " +
		"pages 1870–1879, Vancouver, Canada, July 2017. doi: 10.18653/v1/P17-1171."

	refs := references(text)
	if len(refs) != 1 || !strings.HasPrefix(refs[0].Label, "[5] Danqi Chen, Adam Fisch, Jason Weston, and Antoine Bordes. Reading Wikipedia") ||
		!strings.HasSuffix(refs[0].Label, " …") {
		t.Errorf("references = %+v", refs)
	}
}

func TestABareLinkGetsTheLabelOfItsCitation(t *testing.T) {
	text := "[https://example.org/paper.pdf](https://example.org/paper.pdf)\n\n[3] A. Author. The paper. URL https://example.org/paper.pdf."

	if got := references(text); len(got) != 1 || got[0].Label != "[3] A. Author. The paper. URL https://example.org/paper.pdf." {
		t.Errorf("references = %+v", got)
	}
}

func TestALongParagraphsLabelIsCutAroundTheLink(t *testing.T) {
	text := strings.Repeat("filler words before ", 20) + "the dataset [ImageNet](https://image-net.org/) and " +
		strings.Repeat("filler words after ", 20)

	refs := references(text)
	if len(refs) != 1 {
		t.Fatalf("references = %+v", refs)
	}

	l := refs[0].Label
	if !strings.HasPrefix(l, "… ") || !strings.HasSuffix(l, " …") || !strings.Contains(l, "the dataset ImageNet and") ||
		len([]rune(l)) > maxLabelRunes+4 || strings.Contains(l, "  ") {
		t.Errorf("label = %q", l)
	}
}

func TestReferencesSkipWhatIsNoWebLink(t *testing.T) {
	text := "[Chapter I](132-h-2.htm.html#chap01) and [](cover.jpg), mailto:someone@example.com, " +
		"version 10.5 of the tool, and https://www.gutenberg.org twice: https://www.gutenberg.org."

	if got := references(text); len(got) != 1 || got[0] != (data.Reference{URL: "https://www.gutenberg.org", Label: got[0].Label}) {
		t.Errorf("references = %+v", got)
	}
}
