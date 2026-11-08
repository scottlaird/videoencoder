package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// command describes one of videoencoder's modes for help output.
type command struct {
	name    string // as typed; "render" is the default, typed as nothing
	summary string // one line, for the command list
	about   string // a paragraph, for the command's own help
	flags   []string
}

// argsHelp explains the arguments every command takes.
const argsHelp = `Arguments are directories (default "."), searched for videos defined by
<name>.video.yaml sidecars or videos: entries in videoencoder.yaml files;
sidecars, each naming one video; or media files, which are versions of one
ad hoc video, such as its HDR and SDR masters.`

var commands = []command{
	{
		name:    "render",
		summary: "encode the renditions that changed, package the stream, and publish it",
		about: `Encodes every rendition whose source or settings changed, packages them
into DASH and HLS manifests with posters and seek thumbnails, and publishes
the stream to the config's publish.to targets.`,
		flags: []string{"config", "rebuild", "only", "outdir", "streamdir", "thumbnails", "publish", "to", "prune", "tui"},
	},
	{
		name:    "plan",
		summary: "show each video's merged config and what would be rebuilt; encode nothing",
		about: `Prints each video's merged config, with a comment on each value saying
which file it came from, then lists every output and whether it would be
rebuilt and why. Encodes nothing.`,
		flags: []string{"config", "rebuild", "only", "outdir", "streamdir", "thumbnails"},
	},
	{
		name:    "tune",
		summary: "find each rendition's quality for the target VMAF and write it to the sidecar",
		about: `Encodes short samples of each video to find the quality setting each
rendition needs to reach tune.target_vmaf, and writes the results into
the video's sidecar. Review them, then render.`,
		flags: []string{"config", "jobs", "outdir", "tui"},
	},
	{
		name:    "publish",
		summary: "copy streams already rendered to their publish targets",
		about: `Copies each video's finished stream to its publish.to targets, without
rendering. Only files that changed are uploaded.`,
		flags: []string{"config", "to", "prune", "streamdir", "tui"},
	},
}

func lookupCommand(name string) (command, bool) {
	i := slices.IndexFunc(commands, func(c command) bool { return c.name == name })
	if i < 0 {
		return command{}, false
	}
	return commands[i], true
}

// printUsage writes help for mode, or for videoencoder as a whole when
// mode is "render" and overview is set, describing the flags in fs.
func printUsage(w io.Writer, fs *flag.FlagSet, mode string, overview bool) {
	c, _ := lookupCommand(mode)
	if overview {
		fmt.Fprint(w, `Usage:
  videoencoder [flags] [dir | name.video.yaml | media files ...]
  videoencoder <command> [flags] [same arguments]
  videoencoder help [command]

videoencoder turns master video files into adaptive DASH and HLS streams in
H.264, H.265 and AV1, rebuilding only what changed.

Commands:
`)
		for _, c := range commands {
			name := c.name
			if name == "render" {
				name = "(none)"
			}
			fmt.Fprintf(w, "  %-8s  %s\n", name, c.summary)
		}
		fmt.Fprintf(w, "\n%s\n\nFlags without a command (render):\n", argsHelp)
	} else {
		name := " " + c.name
		if c.name == "render" {
			name = ""
		}
		fmt.Fprintf(w, "Usage: videoencoder%s [flags] [dir | name.video.yaml | media files ...]\n\n%s\n\n%s\n\nFlags:\n", name, c.about, argsHelp)
	}
	printFlags(w, fs, c.flags)
	if overview {
		fmt.Fprint(w, "\nRun \"videoencoder help <command>\" for a command's flags.\n")
	}
}

// flagArgs name what each flag that takes a value expects.
var flagArgs = map[string]string{
	"config": "FILE", "rebuild": "GLOB", "only": "GLOB", "outdir": "DIR",
	"streamdir": "DIR", "to": "TARGET,...", "jobs": "N", "tui": "MODE",
}

// printFlags lists the named flags of fs, in order, as --name VALUE.
func printFlags(w io.Writer, fs *flag.FlagSet, names []string) {
	for _, n := range names {
		f := fs.Lookup(n)
		if f == nil {
			continue
		}
		arg, usage := flag.UnquoteUsage(f)
		if a, ok := flagArgs[f.Name]; ok {
			arg = a
		}
		head := "--" + f.Name
		if arg != "" {
			head += " " + strings.ToUpper(arg)
		}
		if def := f.DefValue; def != "" && def != "false" && def != "0" {
			usage += fmt.Sprintf(" (default %s)", def)
		}
		fmt.Fprintf(w, "  %s\n      %s\n", head, usage)
	}
}

// checkFlags returns an error naming a flag set in fs that mode
// doesn't use, since it would be silently ignored.
func checkFlags(fs *flag.FlagSet, mode string) error {
	c, _ := lookupCommand(mode)
	var err error
	fs.Visit(func(f *flag.Flag) {
		if err == nil && !slices.Contains(c.flags, f.Name) {
			name := c.name
			if name == "render" {
				name = "rendering"
			}
			err = fmt.Errorf("--%s doesn't apply to %s; see \"videoencoder help %s\"", f.Name, name, c.name)
		}
	})
	return err
}
