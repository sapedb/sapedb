package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// renderReport turns the JSON lines the run collected into the Markdown that
// gets read by a person.
//
// It is in this binary rather than in the shell script for one reason: the
// script writes those lines and must never have to parse them. A shell that
// parses JSON needs a JSON parser, and the rule for this rig is nothing that
// is not already here or in a standard image.
func renderReport(in, out string) error {
	if in == "" || out == "" {
		return fmt.Errorf("the report needs -in and -out")
	}
	file, err := os.Open(in)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	var records []record
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for lines.Scan() {
		text := strings.TrimSpace(lines.Text())
		if text == "" {
			continue
		}
		var one record
		if err := json.Unmarshal([]byte(text), &one); err != nil {
			return fmt.Errorf("reading %s: %w: %s", in, err, trim(text))
		}
		records = append(records, one)
	}
	if err := lines.Err(); err != nil {
		return err
	}

	page := build(records)
	if err := os.WriteFile(out, []byte(page), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d records)\n", out, len(records))
	return nil
}

// profileOrder keeps the profiles in the order of the ladder rather than
// alphabetically, so a reader sees the curve.
var profileOrder = map[string]int{"tiny": 0, "small": 1, "medium": 2}

func build(records []record) string {
	var page strings.Builder

	var host record
	for _, one := range records {
		if one.Kind == "host" {
			host = one
		}
	}

	profiles := map[string]bool{}
	for _, one := range records {
		if one.Profile != "" {
			profiles[one.Profile] = true
		}
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, aKnown := profileOrder[names[i]]
		b, bKnown := profileOrder[names[j]]
		if aKnown && bKnown {
			return a < b
		}
		return names[i] < names[j]
	})

	commit := ""
	server := ""
	for _, one := range records {
		if one.Commit != "" {
			commit = one.Commit
		}
		if one.Server != "" {
			server = one.Server
		}
	}

	fmt.Fprintf(&page, "# sapedb under a small VPS's limits — one machine, one day\n\n")
	fmt.Fprintf(&page, "Every number here is one reading from one machine on one day. It is a shape, not a promise.\n")
	fmt.Fprintf(&page, "Nothing in this file is a claim about what sapedb is fast at; it is a record of what this\n")
	fmt.Fprintf(&page, "workload cost on these limits, at the layer named below, so the next measurement has\n")
	fmt.Fprintf(&page, "something to disagree with.\n\n")

	fmt.Fprintf(&page, "## Conditions\n\n")
	fmt.Fprintf(&page, "| | |\n|---|---|\n")
	fmt.Fprintf(&page, "| measured on | %s |\n", orBlank(host.Date))
	fmt.Fprintf(&page, "| image built from commit | `%s` |\n", orBlank(commit))
	if server != "" {
		fmt.Fprintf(&page, "| server said it was | `%s` |\n", server)
	}
	fmt.Fprintf(&page, "| host | %s |\n", orBlank(host.Host))
	fmt.Fprintf(&page, "| host kernel | %s |\n", orBlank(host.Kernel))
	fmt.Fprintf(&page, "| docker | %s, %s/%s |\n", orBlank(host.Docker), orBlank(host.DockerOS), orBlank(host.Arch))
	fmt.Fprintf(&page, "| the VM docker runs in | %s CPUs, %s bytes of RAM |\n", orBlank(host.VMCPUs), orBlank(host.VMMemoryBytes))
	fmt.Fprintf(&page, "| transport | plaintext TCP (`SAPEDB_INSECURE=1`) |\n")
	fmt.Fprintf(&page, "| encryption at rest | off (`SAPEDB_ENCRYPT` unset) |\n")
	fmt.Fprintf(&page, "| concurrency | one connection, one request outstanding at a time |\n")
	fmt.Fprintf(&page, "\n")

	fmt.Fprintf(&page, "### The caveat that is not a footnote\n\n")
	fmt.Fprintf(&page, "This ran under Docker Desktop on macOS, which is a Linux virtual machine with a\n")
	fmt.Fprintf(&page, "virtualised disk. **Its disk behaviour is not a VPS's.** `fsync` in that VM does not\n")
	fmt.Fprintf(&page, "mean what `fsync` on a cloud provider's network-attached block device means, and this\n")
	fmt.Fprintf(&page, "store commits per write — so the write numbers below are the ones most exposed to\n")
	fmt.Fprintf(&page, "that difference, and could move by an order of magnitude in either direction on real\n")
	fmt.Fprintf(&page, "hardware. That is the reason these numbers are a shape rather than a promise. Read\n")
	fmt.Fprintf(&page, "the ratios between profiles, and the ratio between insert and delete; do not quote the\n")
	fmt.Fprintf(&page, "absolute rows/second at anybody.\n\n")

	fmt.Fprintf(&page, "## The profiles\n\n")
	fmt.Fprintf(&page, "Limits read back off the running container with `docker inspect`, not copied from the\n")
	fmt.Fprintf(&page, "compose file — a limit that did not apply is the single easiest way for a run like this\n")
	fmt.Fprintf(&page, "to be worthless.\n\n")
	fmt.Fprintf(&page, "| profile | asked for | memory applied | swap applied | swap disabled | cpu applied |\n")
	fmt.Fprintf(&page, "|---|---|---|---|---|---|\n")
	// One row per profile, even though a profile that was also used for the
	// wall was brought up twice and so wrote two identical records.
	for _, name := range names {
		for _, one := range records {
			if one.Kind != "limits" || one.Profile != name {
				continue
			}
			fmt.Fprintf(&page, "| %s | %s cpu, %s | %s | %s | %s | %.2f |\n",
				name, orBlank(one.AskedCPUs), orBlank(one.AskedMemory),
				mib(one.MemoryBytes), mib(one.MemorySwapBytes),
				yesNo(one.MemorySwapBytes == one.MemoryBytes),
				float64(one.NanoCPUs)/1e9)
			break
		}
	}
	fmt.Fprintf(&page, "\n`memswap_limit` equals `mem_limit` on every profile, which is what turns the memory\n")
	fmt.Fprintf(&page, "limit into a wall instead of a hint. Without it the container swaps, the numbers look\n")
	fmt.Fprintf(&page, "fine, and they mean nothing.\n\n")

	for _, phase := range []string{"insert", "scan", "delete"} {
		writePhase(&page, records, names, phase)
	}

	writeWall(&page, records)

	fmt.Fprintf(&page, "## How to read the memory column\n\n")
	fmt.Fprintf(&page, "Peak memory is sampled from outside with `docker stats`, about once a second, for the\n")
	fmt.Fprintf(&page, "duration of that phase and nothing else. Two things follow, and both make it a lower\n")
	fmt.Fprintf(&page, "bound rather than a measurement of the true peak: a spike between two samples is not\n")
	fmt.Fprintf(&page, "seen at all, and the process's last moments before an out-of-memory kill are exactly\n")
	fmt.Fprintf(&page, "the moments most likely to fall between them. What `docker stats` reports is the\n")
	fmt.Fprintf(&page, "cgroup's current usage less its inactive file cache, so it is closer to what the\n")
	fmt.Fprintf(&page, "process is holding than to what the kernel has charged the cgroup — but it is not the\n")
	fmt.Fprintf(&page, "same number the OOM killer looks at.\n\n")

	return page.String()
}

func writePhase(page *strings.Builder, records []record, names []string, phase string) {
	title := map[string]string{
		"insert": "Insert: 2 KB rows, one at a time",
		"scan":   "Paginated prefix scan: the application's read path",
		"delete": "Delete, one row per request",
	}[phase]
	fmt.Fprintf(page, "## %s\n\n", title)

	var note string
	var shape string
	for _, one := range records {
		if one.Kind == "phase" && one.Phase == phase {
			note = one.Note
			shape = fmt.Sprintf("%d timed readings, each of %d rows of %d base64 characters",
				one.Rate.Runs, one.RowsPerRun, one.RowBytes)
			if phase == "scan" && one.PageSize > 0 {
				shape += fmt.Sprintf(", paged %d rows at a time", one.PageSize)
			}
		}
	}
	if shape != "" {
		fmt.Fprintf(page, "%s. %s\n\n", shape, note)
	}

	fmt.Fprintf(page, "| profile | rows/s min | rows/s median | rows/s max | request ms median | request ms p99 | request ms max | peak container memory |\n")
	fmt.Fprintf(page, "|---|---|---|---|---|---|---|---|\n")
	for _, name := range names {
		var row *record
		for i := range records {
			if records[i].Kind == "phase" && records[i].Phase == phase && records[i].Profile == name {
				row = &records[i]
			}
		}
		if row == nil {
			continue
		}
		peak := "not sampled"
		for _, one := range records {
			if one.Kind != "peak" || one.Profile != name || one.Phase != phase {
				continue
			}
			if one.PeakSamples == 0 {
				// A phase shorter than the sampler's interval. Saying "0 MiB"
				// here would be a measurement of nothing dressed as a
				// measurement of something.
				peak = "phase ended before a sample landed"
				continue
			}
			peak = fmt.Sprintf("%s (%d samples)", mib(one.PeakBytes), one.PeakSamples)
		}
		fmt.Fprintf(page, "| %s | %.0f | %.0f | %.0f | %.2f | %.2f | %.2f | %s |\n",
			name, row.Rate.Min, row.Rate.Median, row.Rate.Max,
			row.Latency.MedianMS, row.Latency.P99MS, row.Latency.MaxMS, peak)
	}
	fmt.Fprintf(page, "\n")

	// The readings in the order they were taken, where there are few enough to
	// read. A spread cannot tell a noisy phase from one that gets steadily
	// worse, and which of the two this is matters more than either endpoint.
	series := false
	for _, one := range records {
		if one.Kind != "phase" || one.Phase != phase || len(one.Series) == 0 {
			continue
		}
		if !series {
			fmt.Fprintf(page, "Each profile's readings in the order they were taken, rows/second:\n\n")
			series = true
		}
		fmt.Fprintf(page, "- %s:", one.Profile)
		for _, reading := range one.Series {
			fmt.Fprintf(page, " %.0f", reading)
		}
		fmt.Fprintf(page, "\n")
	}
	if series {
		fmt.Fprintf(page, "\n")
	}
}

func writeWall(page *strings.Builder, records []record) {
	var wall *record
	for i := range records {
		if records[i].Kind == "wall" {
			wall = &records[i]
		}
	}
	if wall == nil {
		return
	}

	fmt.Fprintf(page, "## What happens at the wall\n\n")
	fmt.Fprintf(page, "One profile — **%s** — was pushed until it stopped. The workload is the mistake an\n", wall.Profile)
	fmt.Fprintf(page, "application actually makes: a scan declared once, with a limit far above what the\n")
	fmt.Fprintf(page, "collection held at the time, asked for again after the collection has grown. Each rung\n")
	fmt.Fprintf(page, "adds rows and then asks for all of them at once.\n\n")

	fmt.Fprintf(page, "| rows in the collection | about | seconds | what the server did |\n|---|---|---|---|\n")
	for _, rung := range wall.Rungs {
		detail := rung.Outcome
		switch rung.Outcome {
		case "answered":
			detail = fmt.Sprintf("answered with %d rows", rung.Returned)
		case "refused":
			detail = "refused: " + rung.Detail
		case "hung_up":
			detail = "hung up mid-answer, and was still serving afterwards: " + rung.Detail
		case "died_reading":
			detail = "**did not come back, and the server was gone**: " + rung.Detail
		case "died_writing":
			detail = "**died while the rows were being written**: " + rung.Detail
		}
		fmt.Fprintf(page, "| %d | %s | %.1f | %s |\n", rung.Rows, mib(rung.Bytes), rung.Seconds, detail)
	}
	fmt.Fprintf(page, "\n**Verdict: %s.** The server was %s when the phase ended.\n\n%s\n\n",
		wall.Verdict, orBlank(wall.Alive), orBlank(wall.Note))

	for _, one := range records {
		if one.Kind == "death" {
			fmt.Fprintf(page, "What docker says about that container afterwards: status `%s`, exit code `%s`, OOM-killed `%s`, restarts `%s`.\n\n",
				orBlank(one.Status), orBlank(one.ExitCode), orBlank(one.OOMKilled), orBlank(one.Restarts))
			fmt.Fprintf(page, "The restart count matters for reading the table above. A rung that says the server\n")
			fmt.Fprintf(page, "hung up and was serving afterwards would be a lie if something had restarted the\n")
			fmt.Fprintf(page, "process in between; `restart: \"no\"` in the compose file is why it cannot, and this\n")
			fmt.Fprintf(page, "is the reading that says it did not.\n\n")
		}
		if one.Kind == "peak" && one.Phase == "wall" {
			fmt.Fprintf(page, "Peak sampled during the wall phase: %s over %d samples — a lower bound, for the reason below.\n\n",
				mib(one.PeakBytes), one.PeakSamples)
		}
	}
}

func mib(bytes int64) string {
	if bytes == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
}

func yesNo(yes bool) string {
	if yes {
		return "yes"
	}
	return "**NO — this run is not trustworthy**"
}

func orBlank(text string) string {
	if text == "" {
		return "—"
	}
	return text
}
