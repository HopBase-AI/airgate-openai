package gateway

// 2026-09-10:一位西班牙语客户在 API 响应里收到了中文报错。凡是能到达外部 API 客户端的文本
// (jsonError / openAIErrorJSON / anthropicErrorJSON* / buildImagesErrorBody* / writeJSONOutcome /
// writeSSEError / *Outcome 的 reason、ForwardOutcome.Reason、TaskError.Message,以及被这些路径
// 透传 err.Error() 的 fmt.Errorf / errors.New)一律必须是英文;本地化由 core 按错误码处理,
// 插件不负责。本测试用 go/ast 扫描包内所有非测试源码,发现上述位置出现汉字字面量即失败。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// clientErrorSinkFuncs 是客户端可见文本的直接出口函数。
var clientErrorSinkFuncs = map[string]bool{
	"jsonError":                    true,
	"openAIErrorJSON":              true,
	"anthropicErrorJSON":           true,
	"anthropicErrorJSONWithCode":   true,
	"buildImagesErrorBody":         true,
	"buildImagesErrorBodyWithCode": true,
	"writeJSONOutcome":             true,
	"writeSSEError":                true,
	"failureOutcome":               true,
	"transientOutcome":             true,
	"streamAbortedOutcome":         true,
	"accountDeadOutcome":           true,
}

// clientErrorSinkFields 是客户端可见文本的结构体字段(sdk.ForwardOutcome.Reason / TaskError.Message 等)。
var clientErrorSinkFields = map[string]bool{
	"Reason":  true,
	"Message": true,
}

// clientErrorGuardSkipFiles 只服务后台控制台(管理员 OAuth 流程 / 元数据),不经外部 API 返回。
var clientErrorGuardSkipFiles = map[string]bool{
	"metadata.go":      true,
	"oauth.go":         true,
	"oauth_handler.go": true,
}

// clientErrorGuardSkipFuncs 是 gateway.go 内只被后台控制台调用的方法(账号校验 / 额度查询 / 管理操作)。
var clientErrorGuardSkipFuncs = map[string]bool{
	"HandleRequest":              true,
	"ValidateAccount":            true,
	"validateAPIKeyViaModels":    true,
	"validateAPIKeyViaResponses": true,
	"QueryQuota":                 true,
}

func TestClientFacingErrorTextMustBeEnglish(t *testing.T) {
	dirs := []string{".", "imgen"}
	fset := token.NewFileSet()
	var violations []string

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if clientErrorGuardSkipFiles[name] {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && clientErrorGuardSkipFuncs[fn.Name.Name] {
					continue
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.CallExpr:
						if isClientErrorSinkCall(node) {
							for _, arg := range node.Args {
								if lit := firstHanLiteral(arg); lit != nil {
									violations = append(violations, fset.Position(lit.Pos()).String()+": "+lit.Value)
								}
							}
						}
					case *ast.KeyValueExpr:
						if key, ok := node.Key.(*ast.Ident); ok && clientErrorSinkFields[key.Name] {
							if lit := firstHanLiteral(node.Value); lit != nil {
								violations = append(violations, fset.Position(lit.Pos()).String()+": "+lit.Value)
							}
						}
					}
					return true
				})
			}
		}
	}

	if len(violations) > 0 {
		t.Fatalf("client-facing error text must be English (localization happens in core by error code); found Han characters at:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

func isClientErrorSinkCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return clientErrorSinkFuncs[fn.Name]
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		if !ok {
			return false
		}
		switch pkg.Name + "." + fn.Sel.Name {
		case "fmt.Errorf", "errors.New":
			return true
		}
	}
	return false
}

// firstHanLiteral 深度遍历表达式(含字符串拼接与嵌套 fmt.Sprintf),返回第一个含汉字的字符串字面量。
func firstHanLiteral(expr ast.Expr) *ast.BasicLit {
	var found *ast.BasicLit
	ast.Inspect(expr, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			value = lit.Value
		}
		for _, r := range value {
			if unicode.Is(unicode.Han, r) {
				found = lit
				return false
			}
		}
		return true
	})
	return found
}
