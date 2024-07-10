package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gocolly/colly"
	"github.com/romanpickl/pdf"
)

type WebScrape struct {
	LotteryName string `json:"lottery_name"`
	LotteryDate string `json:"lottery_date"`
	PdfLink     string `json:"pdf_link"`
}

type LotteryResults struct {
	LastUpdated time.Time                      `json:"latest_draw"`
	Results     map[string]map[string][]string `json:"results"`
}

var (
	lotteryResults   LotteryResults
	lotteryListCache []WebScrape

	numbersRegex      = regexp.MustCompile(`\d+`)
	alphanumericRegex = regexp.MustCompile(`\[([A-Z]+ \d+)\]`)
	seriesRegex       = regexp.MustCompile(`\[([A-Z])\]`)

	headerPattern             = `KERALA.*?( 1st)`
	footerPattern             = `Page \d  IT Support : NIC Kerala  \d{2}\/\d{2}\/\d{4} \d{2}:\d{2}:\d{2}`
	EndFooterPattern          = `The prize winners?.*`
	trailingWhiteSpacePattern = `\s{2}.\s`
	bulletPattern             = `(?:\d|\d{2})\)`
	podiumSplit               = `FOR +.* NUMBERS`
	lotteryTicketFull         = `[A-Z]{2} \d{6}`
	locationString            = `\(\S+\)`
	prizePositionString       = `((\d(st|rd|nd|th))|Cons)`
	prizeString               = `(Prize Rs :\d+/-)|(Prize-Rs :\d+/-)`
	seriesSelection           = `(?:\[)(.)`

	resultsFile = "results.json"
)

func scheduleDailyCheck() {
	go func() {
		loc, err := time.LoadLocation("Asia/Kolkata")
		if err != nil {
			log.Fatalf("Failed to load IST location: %v", err)
		}
		for {
			now := time.Now().In(loc)
			today3pm := time.Date(now.Year(), now.Month(), now.Day(), 15, 0, 0, 0, loc)
			if now.After(today3pm) {
				checkAndRefreshData()
			}
			time.Sleep(time.Until(today3pm.Add(24 * time.Hour)))
		}
	}()
}

func saveDataToFile(filename string, data interface{}) error {
	jsonData, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, jsonData, 0644)
}

func loadDataFromFile(filename string, data interface{}) error {
	jsonData, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonData, data)
}

func crawlAndSaveResults(firstVisit bool) error {
	lotteryList := getLotteryList(firstVisit)
	if len(lotteryList) == 0 {
		return fmt.Errorf("failed to fetch lottery list")
	}
	lotteryResults.LastUpdated, _ = time.Parse("02/01/2006", lotteryList[0].LotteryDate)
	lotteryListCache = lotteryList

	results := make(map[string]map[string][]string)
	for _, lottery := range lotteryList {
		if lottery.LotteryName == "" {
			continue
		}

		resp, err := http.Get(lottery.PdfLink)
		if err != nil || resp.StatusCode != http.StatusOK {
			log.Printf("Failed to download PDF for %s: %v", lottery.LotteryName, err)
			continue
		}
		defer resp.Body.Close()

		content, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read PDF content for %s: %v", lottery.LotteryName, err)
			continue
		}

		text, err := ExtractTextFromPDFContent(content)
		if err != nil {
			log.Printf("Failed to extract text from PDF for %s: %v", lottery.LotteryName, err)
			continue
		}

		results[lottery.LotteryName] = parseLotteryNumbers(text)
	}

	lotteryResults.Results = results
	if len(lotteryResults.Results) > 0 {
		if err := saveDataToFile(resultsFile, lotteryResults); err != nil {
			log.Printf("Failed to save lottery results: %v", err)
		}
	} else {
		log.Println("No data found, retrying in 10 minutes...")
		time.Sleep(time.Minute * 10)
		return crawlAndSaveResults(firstVisit)
	}
	return nil
}

func checkAndRefreshData() {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		log.Fatalf("Failed to load IST location: %v", err)
	}
	now := time.Now().In(loc)
	today3pm := time.Date(now.Year(), now.Month(), now.Day(), 15, 0, 0, 0, loc)
	if lotteryResults.LastUpdated.Before(today3pm) && now.After(today3pm) {
		log.Println("Data is outdated, refreshing...")
		if err := crawlAndSaveResults(false); err != nil {
			log.Printf("Failed to refresh data: %v", err)
		}
		log.Println("Data has been refreshed")
	} else {
		log.Println("Data is up-to-date")
	}
}

func getLotteryList(firstVisit bool) []WebScrape {
	var datas []WebScrape
	c := colly.NewCollector(colly.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/58.0.3029.110 Safari/537.3"))

	c.OnHTML("tr", func(e *colly.HTMLElement) {
		href := e.ChildAttr("td a", "href")
		text := e.ChildText("td:first-child")
		text2 := e.ChildText("td:nth-child(2)")
		if text != "" {
			datas = append(datas, WebScrape{LotteryName: text, LotteryDate: text2, PdfLink: href})
		}
	})

	if firstVisit {
		c.Visit("https://statelottery.kerala.gov.in/index.php/lottery-result-view")
		return datas
	}

	for {
		c.Visit("https://statelottery.kerala.gov.in/index.php/lottery-result-view")
		if len(datas) == 0 {
			log.Println("Error fetching lottery list, retrying...")
			time.Sleep(time.Minute * 10)
			continue
		}
		latestDate, err := time.Parse("02/01/2006", datas[0].LotteryDate)
		if err == nil && (latestDate.Day() >= time.Now().Local().Day() || lotteryResults.LastUpdated.Day() < latestDate.Day()) {
			lotteryResults.LastUpdated = latestDate
			break
		}
		log.Println("Latest data not available, checking again in 10 minutes...")
		time.Sleep(time.Minute * 10)
	}
	return datas
}

func parseLotteryNumbers(input string) map[string][]string {
	result := make(map[string][]string)
	parts := strings.Split(input, "<")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		pos := strings.TrimSpace(strings.Split(part, ">")[0])
		numbersPart := strings.TrimSpace(strings.SplitN(part, ">", 2)[1])

		seriesMatches := seriesRegex.FindAllStringSubmatch(numbersPart, -1)
		for _, match := range seriesMatches {
			result[pos] = append(result[pos], match[1])
		}

		alphanumericMatches := alphanumericRegex.FindAllStringSubmatch(numbersPart, -1)
		for _, match := range alphanumericMatches {
			result[pos] = append(result[pos], match[1])
		}

		numbersPart = alphanumericRegex.ReplaceAllString(numbersPart, "")
		numbers := numbersRegex.FindAllString(numbersPart, -1)
		for _, num := range numbers {
			for i := 0; i < len(num); i += 4 {
				end := i + 4
				if end > len(num) {
					end = len(num)
				}
				result[pos] = append(result[pos], num[i:end])
			}
		}
	}
	return result
}

func ProcessTextContent(input string) (string, error) {
	patternsToRemove := []string{headerPattern, footerPattern, bulletPattern, EndFooterPattern, trailingWhiteSpacePattern, locationString, podiumSplit, prizeString}
	for _, pattern := range patternsToRemove {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return "", err
		}
		if pattern == headerPattern {
			input = re.ReplaceAllString(input, "1st")
		}
		input = re.ReplaceAllString(input, "")
	}
	input = regexp.MustCompile(prizePositionString).ReplaceAllString(input, ` < $0 > `)
	input = regexp.MustCompile(lotteryTicketFull).ReplaceAllString(input, "[$0]")

	seriesMatches := regexp.MustCompile(seriesSelection).FindAllStringSubmatch(input, -1)
	if len(seriesMatches) > 0 {
		series := seriesMatches[0][1]
		input = fmt.Sprintf(`< Series > [%s] %s`, series, input)
	}
	return input, nil
}

func ExtractTextFromPDFContent(content []byte) (string, error) {
	finalString := ""
	r, err := pdf.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", err
	}

	for pageIndex := 1; pageIndex <= r.NumPage(); pageIndex++ {
		p := r.Page(pageIndex)
		if p.V.IsNull() {
			continue
		}

		rows, _ := p.GetTextByRow()
		for _, row := range rows {
			for _, word := range row.Content {
				finalString += word.S + " "
			}
		}
	}

	return ProcessTextContent(finalString)
}

func getAllResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lotteryResults)
}

func listLotteries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lotteryListCache)
}

func checkTickets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var tickets []string
	if err := json.NewDecoder(r.Body).Decode(&tickets); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	winners := make(map[string]map[string][]string)
	for lotteryName, results := range lotteryResults.Results {
		currentWinners := checkWinningTickets(results, tickets)
		for pos, winningTickets := range currentWinners {
			if winners[pos] == nil {
				winners[pos] = make(map[string][]string)
			}
			winners[pos][lotteryName] = append(winners[pos][lotteryName], winningTickets...)
		}
	}

	json.NewEncoder(w).Encode(winners)
}

func checkWinningTickets(results map[string][]string, tickets []string) map[string][]string {
	winners := make(map[string][]string)
	for _, ticket := range tickets {
		series := string(ticket[0])
		currentSeries := results["Series"]
		if len(currentSeries) == 0 || currentSeries[0] != series {
			continue
		}
		for pos, nums := range results {
			if pos == "Series" {
				continue
			}
			for _, num := range nums {
				if strings.Contains(ticket, num) {
					winners[pos] = append(winners[pos], ticket)
					break
				}
			}
		}
	}
	return winners
}

func main() {
	if err := loadDataFromFile(resultsFile, &lotteryResults); err != nil {
		log.Printf("%s not found or failed to load, running initial crawl...", resultsFile)
		if err := crawlAndSaveResults(true); err != nil {
			log.Fatalf("Failed to crawl and save results: %v", err)
		}
	} else {
		log.Printf("Loaded existing data from %s", resultsFile)
		// Perform a check for new lotteries on startup
		log.Println("Checking for new lotteries on startup...")
		if err := crawlAndSaveResults(false); err != nil {
			log.Printf("Failed to check for new lotteries on startup: %v", err)
		}
	}

	scheduleDailyCheck()

	http.HandleFunc("/results", getAllResults)
	http.HandleFunc("/lotteries", listLotteries)
	http.HandleFunc("/check-tickets", checkTickets)

	log.Println("Starting server on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
