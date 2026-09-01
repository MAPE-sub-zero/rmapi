package shell

import (
	"errors"
	"fmt"
	"strings"

	"github.com/abiosoft/ishell"
)

// ParseTags parses a comma-separated tag string into a slice of non-empty trimmed tag names.
func ParseTags(tagsStr string) []string {
	if strings.TrimSpace(tagsStr) == "" {
		return []string{}
	}
	parts := strings.Split(tagsStr, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	return tags
}

func settagCmd(ctx *ShellCtxt) *ishell.Cmd {
	longHelp := `Usage: settag <path> <tag1,tag2...>`

	return &ishell.Cmd{
		Name:      "settag",
		Help:      "set document tags",
		Completer: createEntryCompleter(ctx),
		LongHelp:  longHelp,
		Func: func(c *ishell.Context) {
			if checkHelp(longHelp, c.Args, c) {
				return
			}

			if len(c.Args) < 2 {
				c.Err(errors.New("missing path and/or tags"))
				return
			}

			targetPath := c.Args[0]
			tagsStr := strings.Join(c.Args[1:], " ")

			node, err := ctx.api.Filetree().NodeByPath(targetPath, ctx.node)
			if err != nil {
				c.Err(errors.New("file doesn't exist"))
				return
			}

			if node.IsRoot() {
				c.Err(errors.New("cannot set tags on root"))
				return
			}

			tags := ParseTags(tagsStr)

			err = ctx.api.UpdateDocumentTags(node.Document.ID, tags)
			if err != nil {
				c.Err(fmt.Errorf("failed to set tags: %w", err))
				return
			}

			node.Document.Tags = tags

			err = ctx.api.SyncComplete()
			if err != nil {
				c.Err(fmt.Errorf("cannot notify: %w", err))
			}
		},
	}
}
