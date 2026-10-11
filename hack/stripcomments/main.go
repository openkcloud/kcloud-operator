// ============================================================
// main.go: Go 파일에서 주석을 뺀 코드만 표준 출력에 쓴다
// 상세: 두 판본의 출력을 비교해 주석만 바뀐 커밋인지 판정하는 데 쓴다. 인자가 "-" 면 표준 입력을 읽는다.
// 생성일: 2026-10-11 | 수정일: 2026-10-11
// ============================================================
package main

import (
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"log"
	"os"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) != 2 {
		log.Fatal("용법: stripcomments <file.go|->")
	}
	var src []byte
	var err error
	if os.Args[1] == "-" {
		src, err = io.ReadAll(os.Stdin)
	} else {
		src, err = os.ReadFile(os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "input.go", src, parser.SkipObjectResolution)
	if err != nil {
		log.Fatal(err)
	}
	if err := printer.Fprint(os.Stdout, fset, f); err != nil {
		log.Fatal(err)
	}
}
