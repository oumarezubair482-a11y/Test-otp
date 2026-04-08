package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/biter777/countries"
	_ "github.com/mattn/go-sqlite3"
	"github.com/nyaruka/phonenumbers"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	client    *whatsmeow.Client
	container *sqlstore.Container
	otpDB     *sql.DB
	dbMutex   sync.Mutex
)

var isFirstRunAPI = true
var apiClient *http.Client

func initClients() {
	apiClient = &http.Client{Timeout: 15 * time.Second}
}

// ================= API Data Structures =================
type APIRecord struct {
	Dt      string `json:"dt"`
	Num     string `json:"num"`
	Cli     string `json:"cli"`
	Message string `json:"message"`
	Payout  string `json:"payout"`
}

type APIResponse struct {
	Status string      `json:"status"`
	Total  int         `json:"total"`
	Data   []APIRecord `json:"data"`
}

// ================= API Fetcher =================
func fetchNumberPanelAPI() ([]APIRecord, bool) {
	// 3 Days Range: Yesterday to Tomorrow
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")

	token := "RlFSSDRSQlaAVXBYim-GdltpbISBZIhGa2F5dIlWa3NfiZVkeXCL"
	
	// Format URL with 3 days range
	fetchURL := fmt.Sprintf("http://51.77.216.195/crapi/konek/viewstats?token=%s&dt1=%s%%2000:00:00&dt2=%s%%2023:59:59&records=50", token, yesterday, tomorrow)

	req, _ := http.NewRequest("GET", fetchURL, nil)
	
	// Request Headers
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", "http://147.135.212.197")
	req.Header.Set("Referer", "http://147.135.212.197/crapi/")

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()

	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, false
	}

	if apiResp.Status != "success" {
		return nil, false
	}

	return apiResp.Data, true
}

// ================= Country Extractor =================
func getCountryFromPhone(phone string) string {
	if !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}
	num, err := phonenumbers.Parse(phone, "")
	if err != nil {
		return "Unknown"
	}
	region := phonenumbers.GetRegionCodeForNumber(num)
	c := countries.ByName(region)
	if c != countries.Unknown {
		return c.Info().Name
	}
	return region
}

// ================= Database Functions =================
func initSQLiteDB() {
	var err error
	otpDB, err = sql.Open("sqlite3", "file:/app/data/kami.db?_foreign_keys=on")
	if err != nil {
		panic(fmt.Sprintf("❌ Failed to open SQLite DB: %v", err))
	}

	createTableQuery := `
	CREATE TABLE IF NOT EXISTS sent_otps (
		msg_id TEXT PRIMARY KEY,
		sent_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	_, err = otpDB.Exec(createTableQuery)
	if err != nil {
		panic(fmt.Sprintf("❌ Failed to create table: %v", err))
	}
	fmt.Println("🗄️ [DB] Local SQLite Database Initialized for Sent OTPs!")
}

func isAlreadySent(id string) bool {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM sent_otps WHERE msg_id = ?)`
	err := otpDB.QueryRow(query, id).Scan(&exists)
	if err != nil {
		return false
	}
	return exists
}

func markAsSent(id string) {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	query := `INSERT OR IGNORE INTO sent_otps (msg_id) VALUES (?)`
	otpDB.Exec(query, id)
}

// ================= Helper Functions =================
func extractOTP(msg string) string {
	re := regexp.MustCompile(`\b\d{3,4}[-\s]?\d{3,4}\b|\b\d{4,8}\b`)
	return re.FindString(msg)
}

func maskPhoneNumber(phone string) string {
	if len(phone) < 6 {
		return phone
	}
	return fmt.Sprintf("%s•••%s", phone[:3], phone[len(phone)-4:])
}

func cleanCountryName(name string) string {
	if name == "" || name == "Unknown" {
		return "Unknown"
	}
	parts := strings.Fields(strings.Split(name, "-")[0])
	if len(parts) > 0 {
		return parts[0]
	}
	return "Unknown"
}

// ================= Test Message Sender =================
func sendTestMessage(cli *whatsmeow.Client) {
	msg := "🟢 *Bot Started Successfully*"
	for _, jidStr := range Config.OTPChannelIDs {
		jid, err := types.ParseJID(jidStr)
		if err != nil { continue }
		
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = cli.SendMessage(ctx, jid, &waProto.Message{
			Conversation: proto.String(msg),
		})
		cancel()

		if err == nil {
			fmt.Printf("✅ [Boot] Test message sent to Channel [%s]\n", jidStr)
		}
	}
}

// ================= Monitoring Loop (Main API) =================
func checkAPIOTPs(cli *whatsmeow.Client) {
	data, success := fetchNumberPanelAPI()

	if !success || len(data) == 0 {
		return
	}

	if isFirstRunAPI {
		fmt.Println("🚀 [Boot] Caching old messages and sending test message...")
		
		// خاموشی سے پرانے میسجز کو سینڈ مارک کر دو تاکہ سپیم نہ ہو
		for _, row := range data {
			msgID := fmt.Sprintf("NP_%v_%v", row.Num, row.Dt)
			markAsSent(msgID)
		}
		
		// چینل میں ٹیسٹ میسج بھیجو
		sendTestMessage(cli)
		isFirstRunAPI = false
		return
	}

	// نیا ڈیٹا چیک کرنے کا لوپ
	for _, row := range data {
		msgID := fmt.Sprintf("NP_%v_%v", row.Num, row.Dt)

		if isAlreadySent(msgID) { continue }

		countryName := getCountryFromPhone(row.Num)
		sendWhatsAppMessage(cli, row.Dt, countryName, row.Num, row.Cli, row.Message, msgID, "API")
	}
}

// ================= Common WhatsApp Sender =================
func sendWhatsAppMessage(cli *whatsmeow.Client, rawTime, countryRaw, phone, service, fullMsg, msgID string, panelSource string) {
	fullMsg = html.UnescapeString(fullMsg)
	fullMsg = strings.ReplaceAll(fullMsg, "null", "")

	reFixN := regexp.MustCompile(`(\d)n([^\d\s])`)
	fullMsg = reFixN.ReplaceAllString(fullMsg, "$1 $2")

	fullMsg = strings.ReplaceAll(fullMsg, "nDont", " Dont")
	fullMsg = strings.ReplaceAll(fullMsg, "nDo ", " Do ")
	fullMsg = strings.ReplaceAll(fullMsg, "nYour", " Your")
	fullMsg = strings.ReplaceAll(fullMsg, "nNe ", " Ne ")
	fullMsg = strings.ReplaceAll(fullMsg, "nلا ", " لا ")

	flatMsg := strings.ReplaceAll(strings.ReplaceAll(fullMsg, "\n", " "), "\r", "")

	if phone == "0" || phone == "" { return }

	cleanCountry := cleanCountryName(countryRaw)
	cFlag, _ := GetCountryWithFlag(cleanCountry) // Requires Config/Flag file

	otpCode := extractOTP(flatMsg)
	maskedPhone := maskPhoneNumber(phone)

	header := fmt.Sprintf("✨ *%s | %s Message* ⚡ [%s]\n\n", cFlag, strings.ToUpper(service), panelSource)

	messageBody := header +
		fmt.Sprintf("> *Time:* %s\n"+
		"> *Country:* %s %s\n"+
		"   *Number:* *%s*\n"+
		"> *Service:* %s\n"+
		"   *OTP:* *%s*\n\n"+
		"> *Join For Numbers:* \n"+
		"> ¹ https://whatsapp.com/channel/0029VbCiwut002TCNTXnqM0t\n"+
		"*Full Message:*\n"+
		"%s\n\n"+
		"> © Developed by Nothing Is Impossible",
		rawTime, cFlag, cleanCountry, maskedPhone, service, otpCode, flatMsg)

	for _, jidStr := range Config.OTPChannelIDs {
		jid, err := types.ParseJID(jidStr)
		if err != nil { continue }

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = cli.SendMessage(ctx, jid, &waProto.Message{
			Conversation: proto.String(strings.TrimSpace(messageBody)),
		})
		cancel()

		if err != nil {
			fmt.Printf("❌ [Send Error] %s: %v\n", phone, err)
		} else {
			fmt.Printf("✅ [Sent] OTP for %s to Channel [%s]\n", phone, jidStr)
		}
		time.Sleep(1 * time.Second)
	}
	markAsSent(msgID)
}

// ================= WhatsApp Events & Handlers =================
func handler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if !v.Info.IsFromMe {
			handleIDCommand(v)
		}
	case *events.LoggedOut:
		fmt.Println("⚠️ [Warn] Logged out from WhatsApp!")
	case *events.Disconnected:
		fmt.Println("❌ [Error] Disconnected! Reconnecting...")
	case *events.Connected:
		fmt.Println("✅ [Info] Connected to WhatsApp")
	}
}

func handlePairAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, `{"error":"Invalid URL format. Use: /link/pair/NUMBER"}`, 400)
		return
	}

	number := strings.TrimSpace(parts[3])
	number = strings.ReplaceAll(number, "+", "")
	number = strings.ReplaceAll(number, " ", "")
	number = strings.ReplaceAll(number, "-", "")

	if len(number) < 10 || len(number) > 15 {
		http.Error(w, `{"error":"Invalid phone number"}`, 400)
		return
	}

	fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("📱 PAIRING REQUEST: %s\n", number)

	if client != nil && client.IsConnected() {
		client.Disconnect()
		time.Sleep(2 * time.Second)
	}

	newDevice := container.NewDevice()
	tempClient := whatsmeow.NewClient(newDevice, waLog.Stdout("Pairing", "INFO", true))
	tempClient.AddEventHandler(handler)

	err := tempClient.Connect()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Connection failed: %v"}`, err), 500)
		return
	}

	time.Sleep(3 * time.Second)

	code, err := tempClient.PairPhone(
		context.Background(),
		number,
		true,
		whatsmeow.PairClientChrome,
		"Chrome (Linux)",
	)

	if err != nil {
		tempClient.Disconnect()
		http.Error(w, fmt.Sprintf(`{"error":"Pairing failed: %v"}`, err), 500)
		return
	}

	fmt.Printf("✅ Code generated: %s\n", code)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

	go func() {
		for i := 0; i < 60; i++ {
			time.Sleep(1 * time.Second)
			if tempClient.Store.ID != nil {
				fmt.Println("✅ Pairing successful!")
				client = tempClient
				return
			}
		}
		tempClient.Disconnect()
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"success": "true",
		"code":    code,
		"number":  number,
	})
}

func handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if client != nil && client.IsConnected() {
		client.Disconnect()
	}

	devices, _ := container.GetAllDevices(context.Background())
	for _, device := range devices {
		device.Delete(context.Background())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"success": "true",
		"message": "Session deleted successfully",
	})
}

func handleIDCommand(evt *events.Message) {
	msgText := ""
	if evt.Message.GetConversation() != "" {
		msgText = evt.Message.GetConversation()
	} else if evt.Message.ExtendedTextMessage != nil {
		msgText = evt.Message.ExtendedTextMessage.GetText()
	}

	if strings.TrimSpace(strings.ToLower(msgText)) == ".id" {
		senderJID := evt.Info.Sender.ToNonAD().String()
		chatJID := evt.Info.Chat.ToNonAD().String()

		response := fmt.Sprintf("👤 *User ID:*\n`%s`\n\n📍 *Chat/Group ID:*\n`%s`", senderJID, chatJID)

		if evt.Message.ExtendedTextMessage != nil && evt.Message.ExtendedTextMessage.ContextInfo != nil {
			quotedID := evt.Message.ExtendedTextMessage.ContextInfo.Participant
			if quotedID != nil {
				cleanQuoted := strings.Split(*quotedID, "@")[0] + "@" + strings.Split(*quotedID, "@")[1]
				cleanQuoted = strings.Split(cleanQuoted, ":")[0]
				response += fmt.Sprintf("\n\n↩️ *Replied ID:*\n`%s`", cleanQuoted)
			}
		}

		if client != nil {
			client.SendMessage(context.Background(), evt.Info.Chat, &waProto.Message{
				Conversation: proto.String(response),
			})
		}
	}
}

// ================= Main Function =================
func main() {
	fmt.Println("🚀 [Init] Starting Kami Bot...")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("✅ Kami Bot is Running! Use /link/pair/NUMBER to pair."))
	})

	http.HandleFunc("/link/pair/", handlePairAPI)
	http.HandleFunc("/link/delete", handleDeleteSession)

	go func() {
		addr := "0.0.0.0:" + port
		fmt.Printf("🌐 API Server listening on %s\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			os.Exit(1)
		}
	}()

	initSQLiteDB()
	initClients()

	dbURL := "file:/app/data/kami_session.db?_foreign_keys=on"
	dbLog := waLog.Stdout("Database", "INFO", true)

	var err error
	container, err = sqlstore.New(context.Background(), "sqlite3", dbURL, dbLog)
	if err == nil {
		deviceStore, err := container.GetFirstDevice(context.Background())
		if err == nil {
			client = whatsmeow.NewClient(deviceStore, waLog.Stdout("Client", "INFO", true))
			client.AddEventHandler(handler)

			if client.Store.ID != nil {
				_ = client.Connect()
				fmt.Println("✅ Session restored")
			}
		}
	}

	// ================= API Loop (Auto-Heal Enabled & 5 Seconds Interval) =================
	go func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("⚠️ [Recovered] API Panel Crash Prevented: %v\n", r)
					}
				}()
				if client != nil && client.IsConnected() && client.IsLoggedIn() {
					checkAPIOTPs(client)
				}
			}()
			// 5 سیکنڈ کے بعد کال ہوگی
			time.Sleep(5 * time.Second) 
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	fmt.Println("\n🛑 Shutting down...")
	if client != nil {
		client.Disconnect()
	}
}