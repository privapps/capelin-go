package app

import (
	"capelin-go/internal/skills"
	"capelin-go/internal/tools"
	"context"
	"net/url"
)

var toolHTTPClient = tools.DefaultHTTPClient()
var ddgSearchURL = tools.DefaultDuckDuckGoURL()
var bingSearchURL = tools.DefaultBingURL()

func syncToolTestOverrides() {
	tools.SetNetworkOverrides(allowPrivateFetch, toolHTTPClient, ddgSearchURL, bingSearchURL)
}

type webSearchArgs = tools.WebSearchArgs
type fetchPageArgs = tools.FetchPageArgs
type listFilesArgs = tools.ListFilesArgs
type readFileArgs = tools.ReadFileArgs
type writeFileArgs = tools.WriteFileArgs
type appendFileArgs = tools.AppendFileArgs
type editFileArgs = tools.EditFileArgs
type executeProgramArgs = tools.ExecuteProgramArgs
type executeSkillArgs = tools.ExecuteSkillArgs

func runFetchPage(ctx context.Context, targetURL string) (string, error) {
	syncToolTestOverrides()
	return tools.RunFetchPage(ctx, targetURL)
}
func validateFetchURL(ctx context.Context, raw string) (*url.URL, error) {
	return tools.ValidateFetchURL(ctx, raw)
}
func runListFiles(root string, yolo bool, args listFilesArgs) (string, error) {
	return tools.RunListFiles(root, yolo, args)
}
func runReadFile(root string, yolo bool, args readFileArgs) (string, error) {
	return tools.RunReadFile(root, yolo, args)
}
func runWriteFile(root string, yolo bool, args writeFileArgs) (string, error) {
	return tools.RunWriteFile(root, yolo, args)
}
func runAppendFile(root string, yolo bool, args appendFileArgs) (string, error) {
	return tools.RunAppendFile(root, yolo, args)
}
func runEditFile(root string, yolo bool, args editFileArgs) (string, error) {
	return tools.RunEditFile(root, yolo, args)
}
func runExecuteProgram(ctx context.Context, root string, yolo bool, args executeProgramArgs) (string, error) {
	return tools.RunExecuteProgram(ctx, root, yolo, args)
}
func runExecuteSkill(ctx context.Context, root string, yolo bool, loaded map[string]skills.Skill, args executeSkillArgs) (string, error) {
	return tools.RunExecuteSkill(ctx, root, yolo, loaded, args)
}
func containsDangerousPattern(command string, args []string) bool {
	return tools.ContainsDangerousPattern(command, args)
}
func resolveWorkspacePath(root, path string) (string, error) {
	return tools.ResolveWorkspacePath(root, path)
}
