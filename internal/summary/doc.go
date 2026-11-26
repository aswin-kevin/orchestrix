// Package summary holds the state the summarize middleware keeps about its
// compactions. It lives outside middleware/summarize so that other packages'
// tests can place a boundary without the key being exported to users, who
// read it with summarize.Current.
package summary
