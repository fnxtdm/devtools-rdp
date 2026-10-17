module devtools-rdp

go 1.21

require github.com/tomatome/grdp v0.0.0

require (
	github.com/huin/asn1ber v0.0.0-20120622192748-af09f62e6358 // indirect
	github.com/icodeface/tls v0.0.0-20190904083142-17aec93c60e5 // indirect
	github.com/lunixbochs/struc v0.0.0-20200707160740-784aaebc1d40 // indirect
	golang.org/x/crypto v0.0.0-20220622213112-05595931fe9d // indirect
)

replace github.com/tomatome/grdp => ./third_party/grdp
