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
		"120363406197025409@newsletter",
	},
	OTPApiURLs: []string{
		"https://RlFSSDRSQlaAVXBYim-GdltpbISBZIhGa2F5dIlWa3NfiZVkeXCL=sms",
		"https://kamina-otp.up.railway.app/d-group/sms",
		"https://kamina-otp.up.railway.app/npm-neon/sms",
		"https://kamina-otp.up.railway.app/mait/sms",
	},
	Interval: 10,
}