// flightlog gera os SVGs do perfil:
//   - dist/night-flight.svg: um avião sobrevoando o gráfico de contribuições à noite,
//     acendendo as "luzes de pista" de cada dia conforme passa.
//   - dist/logbook.svg: o diário de bordo, com as horas de voo a partir de LOGBOOK_START.
//
// Variáveis de ambiente:
//
//	GH_USER           usuário do GitHub (obrigatório)
//	GH_TOKEN          token para a API GraphQL (obrigatório, exceto com MOCK=1)
//	LOGBOOK_START     data de abertura do logbook, AAAA-MM-DD (padrão 2026-11-01)
//	WAKATIME_API_KEY  opcional; quando existe, as horas de voo vêm do WakaTime
//	MOCK=1            gera com dados falsos, para testar localmente
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type day struct {
	Date  time.Time
	Count int
	Level int // 0..4
}

type stats struct {
	Commits      int
	PRs          int
	ActiveDays   int
	Streak       int
	Total        int
	Seconds      float64 // horas de voo (WakaTime)
	HasWakaTime  bool
	Languages    []langTime
	Start, Today time.Time
}

type langTime struct {
	Name    string
	Seconds float64
}

func main() {
	user := os.Getenv("GH_USER")
	if user == "" {
		user = "Gustavo-Oliveira7"
	}
	start, err := time.Parse("2006-01-02", envOr("LOGBOOK_START", "2026-11-01"))
	must(err)
	today := time.Now().UTC().Truncate(24 * time.Hour)

	var weeks [][]day
	st := stats{Start: start, Today: today}

	if os.Getenv("MOCK") == "1" {
		weeks = mockWeeks(today)
		st.Commits, st.PRs, st.ActiveDays, st.Streak = 23, 2, 3, 3
		st.HasWakaTime, st.Seconds = true, 7*3600+42*60
		st.Languages = []langTime{{"Go", 3 * 3600}, {"Java", 2.5 * 3600}, {"TypeScript", 2 * 3600}}
	} else {
		token := os.Getenv("GH_TOKEN")
		if token == "" {
			fail("GH_TOKEN não definido")
		}
		weeks = fetchCalendar(token, user)
		st.ActiveDays, st.Streak = activity(weeks, start)
		// voos = todas as contribuições desde a abertura, igual ao gráfico do perfil
		for _, w := range weeks {
			for _, d := range w {
				if !d.Date.Before(start) {
					st.Commits += d.Count
				}
			}
		}
		if key := os.Getenv("WAKATIME_API_KEY"); key != "" {
			st.Seconds, st.Languages, err = fetchWakaTime(key, start, today)
			if err != nil {
				fmt.Fprintln(os.Stderr, "aviso: WakaTime indisponível:", err)
			} else {
				st.HasWakaTime = true
			}
		}
	}
	for _, w := range weeks {
		for _, d := range w {
			st.Total += d.Count
		}
	}

	must(os.MkdirAll("dist", 0o755))
	write("dist/night-flight.svg", nightFlight(weeks, st))
	write("dist/logbook.svg", logbook(user, st))
	fmt.Println("ok: dist/night-flight.svg, dist/logbook.svg")
}

// ---------- GitHub ----------

func gql(token, query string, vars map[string]any, out any) {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewReader(body))
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct{ Message string }
	}
	must(json.NewDecoder(resp.Body).Decode(&env))
	if len(env.Errors) > 0 {
		fail("GraphQL: " + env.Errors[0].Message)
	}
	must(json.Unmarshal(env.Data, out))
}

func fetchCalendar(token, user string) [][]day {
	const q = `query($u:String!){user(login:$u){contributionsCollection{contributionCalendar{
	  weeks{contributionDays{date contributionCount contributionLevel}}}}}}`
	var r struct {
		User struct {
			ContributionsCollection struct {
				ContributionCalendar struct {
					Weeks []struct {
						ContributionDays []struct {
							Date              string
							ContributionCount int
							ContributionLevel string
						}
					}
				}
			}
		}
	}
	gql(token, q, map[string]any{"u": user}, &r)
	levels := map[string]int{"NONE": 0, "FIRST_QUARTILE": 1, "SECOND_QUARTILE": 2, "THIRD_QUARTILE": 3, "FOURTH_QUARTILE": 4}
	var weeks [][]day
	for _, w := range r.User.ContributionsCollection.ContributionCalendar.Weeks {
		var col []day
		for _, d := range w.ContributionDays {
			t, _ := time.Parse("2006-01-02", d.Date)
			col = append(col, day{t, d.ContributionCount, levels[d.ContributionLevel]})
		}
		weeks = append(weeks, col)
	}
	return weeks
}

func fetchSince(token, user string, from, to time.Time) (commits, prs int) {
	const q = `query($u:String!,$f:DateTime!,$t:DateTime!){user(login:$u){
	  contributionsCollection(from:$f,to:$t){totalCommitContributions totalPullRequestContributions restrictedContributionsCount}}}`
	var r struct {
		User struct {
			ContributionsCollection struct {
				TotalCommitContributions      int
				TotalPullRequestContributions int
				RestrictedContributionsCount  int
			}
		}
	}
	gql(token, q, map[string]any{
		"u": user,
		"f": from.Format(time.RFC3339),
		"t": to.Add(24*time.Hour - time.Second).Format(time.RFC3339),
	}, &r)
	c := r.User.ContributionsCollection
	// contribuições em repos privados entram como "restricted" (sem detalhar o tipo)
	return c.TotalCommitContributions + c.RestrictedContributionsCount, c.TotalPullRequestContributions
}

func activity(weeks [][]day, start time.Time) (active, streak int) {
	var days []day
	for _, w := range weeks {
		days = append(days, w...)
	}
	for _, d := range days {
		if !d.Date.Before(start) && d.Count > 0 {
			active++
		}
	}
	// sequência atual: conta de trás pra frente; hoje sem contribuição ainda não quebra
	for i := len(days) - 1; i >= 0; i-- {
		if days[i].Date.Before(start) {
			break
		}
		if days[i].Count > 0 {
			streak++
		} else if i != len(days)-1 {
			break
		}
	}
	return
}

// ---------- WakaTime ----------

func fetchWakaTime(key string, from, to time.Time) (float64, []langTime, error) {
	url := fmt.Sprintf("https://wakatime.com/api/v1/users/current/summaries?start=%s&end=%s",
		from.Format("2006-01-02"), to.Format("2006-01-02"))
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var r struct {
		CumulativeTotal struct{ Seconds float64 } `json:"cumulative_total"`
		Data            []struct {
			Languages []struct {
				Name         string
				TotalSeconds float64 `json:"total_seconds"`
			}
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, nil, err
	}
	agg := map[string]float64{}
	for _, d := range r.Data {
		for _, l := range d.Languages {
			agg[l.Name] += l.TotalSeconds
		}
	}
	var langs []langTime
	for n, s := range agg {
		langs = append(langs, langTime{n, s})
	}
	sort.Slice(langs, func(i, j int) bool { return langs[i].Seconds > langs[j].Seconds })
	return r.CumulativeTotal.Seconds, langs, nil
}

// ---------- SVG: voo noturno ----------

const (
	cell    = 11
	gap     = 3
	padX    = 24
	gridTop = 64
)

var lights = [5]string{"#1c2433", "#5b4a1f", "#a77b1c", "#e0a526", "#ffd36b"}

func nightFlight(weeks [][]day, st stats) string {
	cols := len(weeks)
	w := padX*2 + cols*(cell+gap) - gap
	h := gridTop + 7*(cell+gap) + 40
	const (
		fly   = 9.0 // segundos para cruzar o grid
		total = 13.0
	)
	pct := func(sec float64) float64 { return sec / total * 100 }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="'SF Mono',Menlo,Consolas,monospace">`, w, h, w, h)
	b.WriteString(`<defs>
<linearGradient id="sky" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#05070d"/><stop offset="1" stop-color="#0d1526"/></linearGradient>
<filter id="glow" x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="1.6" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>
</defs><style>`)

	// cada coluna acende quando o avião passa e apaga junto no fim do ciclo
	for c := 0; c < cols; c++ {
		on := pct(0.6 + fly*float64(c)/float64(cols))
		fmt.Fprintf(&b, "@keyframes k%d{0%%,%.2f%%{opacity:0}%.2f%%,88%%{opacity:1}96%%,100%%{opacity:0}}.c%d{animation:k%d %.0fs linear infinite}\n",
			c, on, on+0.8, c, c, total)
	}
	fmt.Fprintf(&b, `@keyframes fly{0%%{transform:translate(-60px,0)}%.2f%%{transform:translate(%dpx,0)}100%%{transform:translate(%dpx,0)}}
@keyframes bob{0%%,100%%{transform:translateY(0)}50%%{transform:translateY(6px)}}
@keyframes blink{0%%,40%%{opacity:1}50%%,100%%{opacity:.15}}
@keyframes twinkle{0%%,100%%{opacity:.25}50%%{opacity:.9}}
.plane{animation:fly %.0fs linear infinite}.bob{animation:bob 2.2s ease-in-out infinite}
.nav{animation:blink 1s steps(1) infinite}.star{animation:twinkle 3s ease-in-out infinite}
.t{fill:#8b98ad;font-size:11px;letter-spacing:.08em}
</style>`, pct(fly+1.2), w+60, w+60, total)

	fmt.Fprintf(&b, `<rect width="%d" height="%d" rx="10" fill="url(#sky)"/>`, w, h)

	// estrelas (posição fixa por semente, pra não mudar todo dia sem motivo)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 45; i++ {
		fmt.Fprintf(&b, `<circle class="star" cx="%d" cy="%d" r="%.1f" fill="#cfd8ff" style="animation-delay:%.1fs"/>`,
			rng.Intn(w), 6+rng.Intn(gridTop-20), 0.4+rng.Float64()*0.8, rng.Float64()*3)
	}

	fmt.Fprintf(&b, `<text x="%d" y="22" class="t">NIGHT FLIGHT · %d CONTRIBUTIONS IN THE LAST YEAR</text>`, padX, st.Total)

	// pista apagada (base) + luzes por cima
	for c, wk := range weeks {
		x := padX + c*(cell+gap)
		fmt.Fprintf(&b, `<g class="c%d" filter="url(#glow)">`, c)
		var base strings.Builder
		for _, d := range wk {
			y := gridTop + int(d.Date.Weekday())*(cell+gap)
			fmt.Fprintf(&base, `<rect x="%d" y="%d" width="%d" height="%d" rx="2.5" fill="%s"/>`, x, y, cell, cell, lights[0])
			if d.Level > 0 {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2.5" fill="%s"><title>%s: %d</title></rect>`,
					x, y, cell, cell, lights[d.Level], d.Date.Format("02/01/2006"), d.Count)
			}
		}
		b.WriteString("</g>")
		// base vai antes no documento: reescreve inserindo antes do grupo
		s := b.String()
		idx := strings.LastIndex(s, fmt.Sprintf(`<g class="c%d"`, c))
		b.Reset()
		b.WriteString(s[:idx] + base.String() + s[idx:])
	}

	// avião (vista lateral, com luzes de navegação e rastro)
	planeY := gridTop - 30
	fmt.Fprintf(&b, `<g class="plane"><g class="bob"><g transform="translate(0,%d)">`, planeY)
	b.WriteString(`<path d="M-70 1 H-30" stroke="#ffffff" stroke-opacity=".18" stroke-width="1.5" stroke-dasharray="4 5"/>
<path d="M-28 0 C-22 -4 6 -5 16 -2 C22 0 22 3 16 4 C6 6 -22 5 -28 2 Z" fill="#e6edf7"/>
<path d="M-26 -1 L-30 -11 L-23 -11 L-15 -2 Z" fill="#c9d4e6"/>
<path d="M-6 2 L-14 12 L-8 12 L4 3 Z" fill="#b8c4d8"/>
<rect x="9" y="-3" width="5" height="2.2" rx="1" fill="#2b3a55"/>
<circle class="nav" cx="-30" cy="-11" r="1.6" fill="#ff4d4d"/>
<circle class="nav" cx="-11" cy="12" r="1.4" fill="#4dff88" style="animation-delay:.5s"/>`)
	b.WriteString(`</g></g></g>`)

	// legenda
	ly := h - 14
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="t">LESS</text>`, w-padX-150, ly)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="10" height="10" rx="2" fill="%s"/>`, w-padX-112+i*14, ly-9, lights[i])
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="t" text-anchor="end">MORE</text>`, w-padX, ly)
	fmt.Fprintf(&b, `<text x="%d" y="%d" class="t">UPDATED %s UTC</text></svg>`, padX, ly, strings.ToUpper(st.Today.Format("02 Jan 2006")))
	return b.String()
}

// ---------- SVG: logbook ----------

func logbook(user string, st stats) string {
	const w, h = 480, 250
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="'SF Mono',Menlo,Consolas,monospace">`, w, h, w, h)
	b.WriteString(`<style>.l{fill:#8b98ad;font-size:11px;letter-spacing:.08em}.v{fill:#ffd36b;font-size:20px;font-weight:700}.s{fill:#e6edf7;font-size:12px}.h{fill:#e6edf7;font-size:13px;font-weight:700;letter-spacing:.16em}</style>`)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" rx="10" fill="#0b1220" stroke="#1f2a3d"/>`, w, h)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%d" height="44" rx="10" fill="#111b2e"/><rect x="0" y="34" width="%d" height="10" fill="#111b2e"/>`, w, w)
	b.WriteString(`<text x="22" y="28" class="h">✈ PILOT LOGBOOK</text>`)
	fmt.Fprintf(&b, `<text x="%d" y="28" class="l" text-anchor="end">OPENED %s</text>`, w-22, strings.ToUpper(st.Start.Format("02 Jan 2006")))

	hours := "PENDING"
	sub := "avionics (WakaTime) not installed yet"
	if st.HasWakaTime {
		hours = fmt.Sprintf("%dh %02dm", int(st.Seconds)/3600, int(st.Seconds)%3600/60)
		sub = "measured in the editor via WakaTime"
	}
	cells := []struct{ label, value string }{
		{"TOTAL FLIGHT TIME", hours},
		{"FLIGHTS · CONTRIBUTIONS", fmt.Sprint(st.Commits)},
		{"DAYS AIRBORNE", fmt.Sprint(st.ActiveDays)},
		{"CURRENT STREAK", fmt.Sprintf("%d d", st.Streak)},
	}
	for i, c := range cells {
		x := 22 + (i%2)*230
		y := 76 + (i/2)*62
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="l">%s</text><text x="%d" y="%d" class="v">%s</text>`, x, y, c.label, x, y+26, c.value)
	}
	fmt.Fprintf(&b, `<text x="22" y="200" class="l">%s</text>`, strings.ToUpper(sub))

	// type ratings: linguagens com mais horas (só com WakaTime)
	if len(st.Languages) > 0 {
		var parts []string
		for i, l := range st.Languages {
			if i == 4 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %.1fh", l.Name, l.Seconds/3600))
		}
		fmt.Fprintf(&b, `<text x="22" y="226" class="s">TYPE RATINGS · %s</text>`, xmlEsc(strings.Join(parts, " · ")))
	} else {
		fmt.Fprintf(&b, `<text x="22" y="226" class="s">PILOT · @%s</text>`, xmlEsc(user))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// ---------- utilitários ----------

func mockWeeks(today time.Time) [][]day {
	rng := rand.New(rand.NewSource(1))
	start := today.AddDate(0, 0, -364)
	start = start.AddDate(0, 0, -int(start.Weekday()))
	var weeks [][]day
	var col []day
	for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
		n := 0
		if rng.Float64() < 0.45+0.3*math.Sin(float64(d.YearDay())/40) {
			n = rng.Intn(12)
		}
		lvl := 0
		switch {
		case n >= 9:
			lvl = 4
		case n >= 6:
			lvl = 3
		case n >= 3:
			lvl = 2
		case n >= 1:
			lvl = 1
		}
		col = append(col, day{d, n, lvl})
		if d.Weekday() == time.Saturday {
			weeks = append(weeks, col)
			col = nil
		}
	}
	if len(col) > 0 {
		weeks = append(weeks, col)
	}
	return weeks
}

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func write(path, s string) { must(os.WriteFile(filepath.Clean(path), []byte(s), 0o644)) }

func must(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "erro:", msg)
	os.Exit(1)
}
