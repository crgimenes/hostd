package main

import (
	"encoding/json"
	"fmt"

	"github.com/crgimenes/glaze"
	"github.com/crgimenes/glaze/menu"
	"github.com/crgimenes/hostd/version"
)

// The menu bar is what makes this an application rather than a page in a frame.
// The Edit items are not decoration: on macOS, Cmd+C reaches a WKWebView only
// through a menu item wired to the copy: selector, so without this menu the
// operator cannot copy the error the panel is showing them.
//
// The View items drive the page the same way a click does, by calling the
// functions app.js already exposes: the menu is another way to reach the panel,
// never a second implementation of it.
func install(window glaze.WebView) (*menu.Menu, error) {
	call := func(js string) func() {
		return func() { window.Eval(js) }
	}
	about, err := json.Marshal(fmt.Sprintf("hostctl %s · protocol %d · schema %d",
		version.Version, version.Protocol, version.Schema))
	if err != nil {
		return nil, err
	}

	return menu.Set([]menu.Item{
		{Title: "hostctl", Submenu: []menu.Item{
			{Title: "About hostctl", OnClick: call("about(" + string(about) + ")")},
			{Separator: true},
			{Title: "Hide hostctl", Shortcut: "cmd+h", Selector: "hide:"},
			{Title: "Hide Others", Shortcut: "cmd+alt+h", Selector: "hideOtherApplications:"},
			{Title: "Show All", Selector: "unhideAllApplications:"},
			{Separator: true},
			{Title: "Quit hostctl", Shortcut: "cmd+q", OnClick: window.Terminate},
		}},
		{Title: "Edit", Submenu: []menu.Item{
			{Title: "Undo", Shortcut: "cmd+z", Selector: "undo:"},
			{Title: "Redo", Shortcut: "cmd+shift+z", Selector: "redo:"},
			{Separator: true},
			{Title: "Cut", Shortcut: "cmd+x", Selector: "cut:"},
			{Title: "Copy", Shortcut: "cmd+c", Selector: "copy:"},
			{Title: "Paste", Shortcut: "cmd+v", Selector: "paste:"},
			{Title: "Select All", Shortcut: "cmd+a", Selector: "selectAll:"},
			{Separator: true},
			{Title: "Find in Log", Shortcut: "cmd+f", OnClick: call("focusFilter()")},
		}},
		{Title: "View", Submenu: []menu.Item{
			{Title: "Fleet", Shortcut: "cmd+1", OnClick: call("goFleet()")},
			{Separator: true},
			{Title: "Last 5 Minutes", OnClick: call("pickWindow(300)")},
			{Title: "Last Hour", OnClick: call("pickWindow(3600)")},
			{Title: "Last 6 Hours", OnClick: call("pickWindow(21600)")},
			{Title: "Last 24 Hours", OnClick: call("pickWindow(86400)")},
			{Separator: true},
			{Title: "Back to Live", Shortcut: "cmd+l", OnClick: call("goLive()")},
			{Title: "Refresh Now", Shortcut: "cmd+r", OnClick: call("refreshNow()")},
		}},
		{Title: "Window", Submenu: []menu.Item{
			{Title: "Minimize", Shortcut: "cmd+m", Selector: "performMiniaturize:"},
			{Title: "Zoom", Selector: "performZoom:"},
			{Separator: true},
			{Title: "Close", Shortcut: "cmd+w", Selector: "performClose:"},
		}},
	}, menu.Options{Window: window.Window()})
}
