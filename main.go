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
	"github.com/robfig/cron/v3"
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

	contentHeader = "Content-Type"
	contentType   = "application/json"
)

func scheduleDailyCheck() {
	c := cron.New(cron.WithLocation(time.FixedZone("IST", 5*60*60+30*60)))
	_, err := c.AddFunc("15 16 * * *", checkAndRefreshData)
	if err != nil {
		log.Fatalf("Failed to schedule cron job: %v", err)
	}
	c.Start()
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
	lotteryList, err := getLotteryList(firstVisit)
	if err != nil {
		return fmt.Errorf("failed to fetch lottery list: %w", err)
	}
	if len(lotteryList) == 0 {
		return fmt.Errorf("no lottery list found")
	}

	// Update last updated date
	lotteryResults.LastUpdated, _ = time.Parse("02/01/2006", lotteryList[0].LotteryDate)
	lotteryListCache = lotteryList

	// Process lottery results concurrently
	results, err := processLotteryResults(lotteryList)
	if err != nil {
		return err
	}

	// Save results to file
	if err := saveResults(results); err != nil {
		return err
	}

	return nil
}


func processLotteryResults(lotteryList []WebScrape) (map[string]map[string][]string, error) {
	results := make(map[string]map[string][]string)
	resultChan := make(chan struct {
		lotteryName string
		data        map[string][]string
		err         error
	}, len(lotteryList))

	for _, lottery := range lotteryList {
		go func(lottery WebScrape) {
			data, err := processLottery(lottery)
			resultChan <- struct {
				lotteryName string
				data        map[string][]string
				err         error
			}{lotteryName: lottery.LotteryName, data: data, err: err}
		}(lottery)
	}

	for range lotteryList {
		result := <-resultChan
		if result.err != nil {
			log.Printf("Error processing lottery %s: %v", result.lotteryName, result.err)
			continue
		}
		results[result.lotteryName] = result.data
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no results found")
	}

	return results, nil
}


func processLottery(lottery WebScrape) (map[string][]string, error) {
	if lottery.LotteryName == "" {
		return nil, nil
	}

	resp, err := http.Get(lottery.PdfLink)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download PDF for %s: %v", lottery.LotteryName, err)
	}
	defer resp.Body.Close()

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read PDF content for %s: %v", lottery.LotteryName, err)
	}

	text, err := ExtractTextFromPDFContent(content)
	if err != nil {
		return nil, fmt.Errorf("failed to extract text from PDF for %s: %v", lottery.LotteryName, err)
	}

	return parseLotteryNumbers(text), nil
}


func saveResults(results map[string]map[string][]string) error {
	if len(results) == 0 {
		return fmt.Errorf("no results to save")
	}

	lotteryResults.Results = results
	if err := saveDataToFile(resultsFile, lotteryResults); err != nil {
		return fmt.Errorf("failed to save lottery results: %w", err)
	}

	log.Println("Refreshed lottery results")
	return nil
}

func checkAndRefreshData() {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		log.Fatalf("Failed to load IST location: %v", err)
	}
	now := time.Now().In(loc)
	today3pm := time.Date(now.Year(), now.Month(), now.Day(), 16, 15, 0, 0, loc)
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

func getLotteryList(firstVisit bool) ([]WebScrape, error) {
	var datas []WebScrape
	now := time.Now().Local()
	today3pm := time.Date(now.Year(), now.Month(), now.Day(), 16, 15, 0, 0, now.Location())
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
		return datas, nil
	}

	for {
		c.Visit("https://statelottery.kerala.gov.in/index.php/lottery-result-view")
		if len(datas) == 0 {
			log.Println("Error fetching lottery list, retrying...")
			time.Sleep(time.Minute * 10)
			continue
		}
		latestDate, err := time.Parse("02/01/2006", datas[0].LotteryDate)
		if err != nil {
			return nil, err
		} else if latestDate.Day() >= now.Day() || lotteryResults.LastUpdated.Day() < latestDate.Day() {
			lotteryResults.LastUpdated = latestDate
			break
		} else if latestDate.Day() <= now.Day() && now.Before(today3pm) {
			log.Println("current data is up to date...")
			break
		}
		log.Println("Latest data not available, checking again in 15 minutes...")
		time.Sleep(time.Minute * 15)
	}
	return datas, nil
}

func parseLotteryNumbers(input string) map[string][]string {
	result := make(map[string][]string)
	parts := strings.Split(input, "<")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		pos, numbersPart := parsePositionAndNumbersPart(part)
		addSeriesMatches(result, pos, numbersPart)
		addAlphanumericMatches(result, pos, numbersPart)
		addNumericMatches(result, pos, numbersPart)
	}

	return result
}

func parsePositionAndNumbersPart(part string) (string, string) {
	pos := strings.TrimSpace(strings.Split(part, ">")[0])
	numbersPart := strings.TrimSpace(strings.SplitN(part, ">", 2)[1])
	return pos, numbersPart
}

func addSeriesMatches(result map[string][]string, pos, numbersPart string) {
	seriesMatches := seriesRegex.FindAllStringSubmatch(numbersPart, -1)
	for _, match := range seriesMatches {
		result[pos] = append(result[pos], match[1])
	}
}

func addAlphanumericMatches(result map[string][]string, pos, numbersPart string) {
	alphanumericMatches := alphanumericRegex.FindAllStringSubmatch(numbersPart, -1)
	for _, match := range alphanumericMatches {
		result[pos] = append(result[pos], match[1])
	}
}

func addNumericMatches(result map[string][]string, pos, numbersPart string) {
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
	w.Header().Set(contentHeader, contentType)
	json.NewEncoder(w).Encode(lotteryResults)
}

func listLotteries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(contentHeader, contentType)
	json.NewEncoder(w).Encode(lotteryListCache)
}

func checkTickets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(contentHeader, contentType)
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
	series := results["Series"]

	for _, ticket := range tickets {
		if !isMatchingSeries(series, ticket) {
			continue
		}
		checkTicketForWinningPositions(ticket, results, winners)
	}

	return winners
}

func isMatchingSeries(series []string, ticket string) bool {
	return len(series) > 0 && series[0] == string(ticket[0])
}

func checkTicketForWinningPositions(ticket string, results map[string][]string, winners map[string][]string) {
	for pos, nums := range results {
		if pos == "Series" {
			continue
		}
		if isWinningTicket(ticket, nums) {
			winners[pos] = append(winners[pos], ticket)
		}
	}
}

func isWinningTicket(ticket string, nums []string) bool {
	for _, num := range nums {
		if strings.Contains(ticket, num) {
			return true
		}
	}
	return false
}

func main() {
	// Load existing data from file, if available
	err := loadDataFromFile(resultsFile, &lotteryResults)
	if err != nil {
		log.Printf("%s not found or failed to load, attempting initial crawl...", resultsFile)
	} else {
		log.Printf("Loaded existing data from %s", resultsFile)
	}

	// Start the server immediately to serve any available data
	go func() {
		http.HandleFunc("/results", getAllResults)
		http.HandleFunc("/lotteries", listLotteries)
		http.HandleFunc("/check-tickets", checkTickets)

		fs := http.FileServer(http.Dir("./public"))
		http.Handle("/", fs)

		log.Println("Starting server on :8080...")
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	// Perform initial crawl and refresh
	if err == nil {
		log.Println("Checking for new lotteries on startup...")
		if crawlErr := crawlAndSaveResults(false); crawlErr != nil {
			log.Printf("Failed to check for new lotteries on startup: %v", crawlErr)
		}
	} else {
		if crawlErr := crawlAndSaveResults(true); crawlErr != nil {
			log.Fatalf("Failed to crawl and save results: %v", crawlErr)
		}
	}

	// Schedule daily checks using cron
	scheduleDailyCheck()

	// Keep the main goroutine alive
	select {}
}
