package main

var Config = struct {
	OwnerNumber   string
	BotName       string
	OTPChannelIDs []string
	OTPApiURLs    []string
	Interval      int
}{
	OwnerNumber: "923027665767",
	BotName:     "Ali OTP Monitor",
	OTPChannelIDs: []string{
		"120363406828390410@newsletter",
	},
	OTPApiURLs: []string{
		"https://api-kami-nodejs-production.up.railway.app/api?type=sms",
		"https://kamina-otp.up.railway.app/d-group/sms",
		"https://kamina-otp.up.railway.app/npm-neon/sms",
		"https://kamina-otp.up.railway.app/mait/sms",
	},
	Interval: 10,
}