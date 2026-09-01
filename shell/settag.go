package shell

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/abiosoft/ishell"
	"github.com/juruen/rmapi/api/sync15"
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

// settagArgs is the parsed, shell-independent form of a settag invocation.
type settagArgs struct {
	Op               sync15.TagOp
	Tags             []string
	Path             string
	ExpectedRevision string
	JSON             bool
}

// parseSettagArgs parses settag's flags and positional arguments. It does not
// touch the filesystem or the API, so it can be unit-tested without a shell.
func parseSettagArgs(args []string) (*settagArgs, error) {
	opSet := false
	op := sync15.TagOpReplace
	expectedRevision := ""
	jsonOut := false

	positional := make([]string, 0, len(args))

	i := 0
	for i < len(args) {
		arg := args[i]
		switch arg {
		case "--add", "--remove", "--replace":
			if opSet {
				return nil, errors.New("choose one of --add, --remove, --replace")
			}
			opSet = true
			switch arg {
			case "--add":
				op = sync15.TagOpAdd
			case "--remove":
				op = sync15.TagOpRemove
			case "--replace":
				op = sync15.TagOpReplace
			}
		case "--if-revision":
			if i+1 >= len(args) {
				return nil, errors.New("--if-revision needs a revision")
			}
			i++
			expectedRevision = args[i]
		case "--json":
			jsonOut = true
		default:
			positional = append(positional, arg)
		}
		i++
	}

	if len(positional) < 2 {
		return nil, errors.New("missing path and/or tags")
	}

	path := positional[0]
	tagsStr := strings.Join(positional[1:], " ")

	return &settagArgs{
		Op:               op,
		Tags:             ParseTags(tagsStr),
		Path:             path,
		ExpectedRevision: expectedRevision,
		JSON:             jsonOut,
	}, nil
}

func settagCmd(ctx *ShellCtxt) *ishell.Cmd {
	longHelp := `Usage: settag [--add|--remove|--replace] [--if-revision <hash>] [--json] <path> <tag1,tag2...>

  --replace          Make the document's tags exactly the supplied set (default).
  --add              Add the supplied tags, leaving existing ones in place.
  --remove           Remove the supplied tags, leaving the rest in place.
  --if-revision <hash>
                     Only apply the write if the document is currently at this
                     revision hash; otherwise fail rather than overwrite a
                     concurrent change.
  --json             Print the result as indented JSON instead of a summary.

rmapi is licensed under AGPL-3.0.`

	return &ishell.Cmd{
		Name:      "settag",
		Help:      "set document tags",
		Completer: createEntryCompleter(ctx),
		LongHelp:  longHelp,
		Func: func(c *ishell.Context) {
			if checkHelp(longHelp, c.Args, c) {
				return
			}

			settagArgs, err := parseSettagArgs(c.Args)
			if err != nil {
				c.Err(err)
				return
			}

			node, err := ctx.api.Filetree().NodeByPath(settagArgs.Path, ctx.node)
			if err != nil {
				c.Err(errors.New("file doesn't exist"))
				return
			}

			if node.IsRoot() {
				c.Err(errors.New("cannot set tags on root"))
				return
			}

			result, err := ctx.api.UpdateDocumentTagsWithOptions(
				node.Document.ID, settagArgs.Op, settagArgs.Tags, settagArgs.ExpectedRevision)
			if err != nil {
				c.Err(fmt.Errorf("failed to set tags: %w", err))
				return
			}

			node.Document.Tags = result.AfterTags

			if settagArgs.JSON {
				out, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					c.Err(fmt.Errorf("failed to marshal result: %w", err))
					return
				}
				c.Println(string(out))
			} else if !result.Changed {
				c.Printf("unchanged: %s already has tags [%s]\n", settagArgs.Path, strings.Join(result.AfterTags, ", "))
			} else {
				c.Printf("%s: %s\n", result.Operation, settagArgs.Path)
				c.Printf("  tags: [%s] -> [%s]\n", strings.Join(result.BeforeTags, ", "), strings.Join(result.AfterTags, ", "))
				c.Printf("  revision: %s -> %s\n", result.BeforeRevision, result.AfterRevision)
				c.Printf("  page tags preserved: %d\n", result.PageTagCount)
			}

			if result.Changed {
				err = ctx.api.SyncComplete()
				if err != nil {
					c.Err(fmt.Errorf("cannot notify: %w", err))
				}
			}
		},
	}
}
