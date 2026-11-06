// Package allcommands imports every command plugin for its registration side
// effect. Import it (blank) from main so all commands self-register.
//
// Adding a command is: create the folder + add one blank import line here.
//
// Note: the ".help" index lists commands in registration order, which the Go
// runtime fixes as lexicographic import-path order (not the source order below),
// so the index is alphabetical by trigger.
package allcommands

import (
	_ "github.com/stempit/meshcore-go-bot/internal/commands/agent"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/bbs"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/dice"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/eightball"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/hang"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/help"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/last"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/llm"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/news"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/num"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/path"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/ping"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/poker"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/room"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/stat"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/ticket"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/top"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/trace"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/vote"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/weather"
	_ "github.com/stempit/meshcore-go-bot/internal/commands/words"
)
