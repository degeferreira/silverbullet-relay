// Package hub composes Relay routes; Daily/Zotero implementation is owned by Paperwork.
package hub

import "github.com/degeferreira/paperwork/go/integration"

type TaskLine = integration.TaskLine
type Service = integration.Service
type UpstreamError = integration.UpstreamError
type Object = integration.Object
type State = integration.State
type Error = integration.Error
type Settings = integration.Settings
type GoogleCalendar = integration.GoogleCalendar

var (
	Digest            = integration.Digest
	SafePath          = integration.SafePath
	SafeText          = integration.SafeText
	HTMLText          = integration.HTMLText
	APAMarkdown       = integration.APAMarkdown
	PythonJSON        = integration.PythonJSON
	BlockText         = integration.BlockText
	SplitFrontmatter  = integration.SplitFrontmatter
	ManagedNote       = integration.ManagedNote
	LiteratureNote    = integration.LiteratureNote
	MergeProperties   = integration.MergeProperties
	TaskLines         = integration.TaskLines
	TaskProperties    = integration.TaskProperties
	NotePath          = integration.NotePath
	CitationKey       = integration.CitationKey
	NewService        = integration.NewService
	OpenState         = integration.OpenState
	Conflict          = integration.Conflict
	NotConfigured     = integration.NotConfigured
	Uncertain         = integration.Uncertain
	Authors           = integration.Authors
	Year              = integration.Year
	PrivateWrite      = integration.PrivateWrite
	ValidateURL       = integration.ValidateURL
	DefaultSettings   = integration.DefaultSettings
	LoadConfig        = integration.LoadConfig
	NewGoogleCalendar = integration.NewGoogleCalendar
	DailyPattern      = integration.DailyPattern
	str               = integration.String
	obj               = integration.ObjectValue
	num               = integration.Number
	boolean           = integration.Boolean
	array             = integration.Array
	equal             = integration.Equal
	invalid           = integration.Invalid
	defaultInt        = integration.DefaultInt
	enabled           = integration.Enabled
	wait              = integration.Wait
	readObject        = integration.ReadObject
)
