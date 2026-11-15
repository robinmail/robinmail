#!/bin/sh
# Adds sample events and contacts to the development user over CalDAV and
# CardDAV, for demos and screenshots. The events fall in the current week,
# so the week view always has something in it. Run it against a running
# development server (contrib/dev/seed.sh first, then serve):
#
#   contrib/dev/seed-pim.sh [http://127.0.0.1:8080]
#
# Running it again replaces the same events and contacts: every object has
# a fixed name. Events have no guests, so nothing is sent.
set -eu

BASE=${1:-http://127.0.0.1:8080}
USER_ADDRESS=${ROBINMAIL_DEV_USER:-dev@robinmail.test}
PASSWORD=${ROBINMAIL_DEV_PASSWORD:-robinmail-dev}

dav() { curl -sS -u "$USER_ADDRESS:$PASSWORD" "$@"; }
first_href() { grep -o '<d:href>[^<]*</d:href>' | sed 's/<[^>]*>//g' | grep -E "$1" | head -1; }

home_of() { # calendars | addressbooks
	principal=$(dav -X PROPFIND -H 'Depth: 1' "$BASE/dav/" | first_href '/dav/principals/.+/')
	account=$(basename "$principal")
	dav -X PROPFIND -H 'Depth: 1' "$BASE/dav/$1/$account/" | first_href "/dav/$1/$account/$2/"
}
calendar=$(home_of calendars 'c[0-9]+')
book=$(home_of addressbooks 'b[0-9]+')
[ -n "$calendar" ] && [ -n "$book" ] || { echo "no calendar or address book found at $BASE" >&2; exit 1; }

# day N: day N of this week, Monday being 1, as YYYYMMDD (GNU or BSD date).
dow=$(date +%u)
day() {
	n=$(($1 - dow))
	case $n in -*) ;; *) n=+$n ;; esac
	date -d "$n days" +%Y%m%d 2>/dev/null || date -v"${n}d" +%Y%m%d
}
stamp=$(date -u +%Y%m%dT%H%M%SZ)
TZID=Europe/Amsterdam

put_event() { # uid day start end summary location description [rrule]
	start=$(day "$2")
	{
		printf 'BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//robinmail//dev seed//EN\r\nBEGIN:VEVENT\r\n'
		printf 'UID:%s@seed.robinmail.test\r\nDTSTAMP:%s\r\n' "$1" "$stamp"
		if [ "$3" = allday ]; then
			printf 'DTSTART;VALUE=DATE:%s\r\nDTEND;VALUE=DATE:%s\r\n' "$start" "$(day $(($2 + 1)))"
		else
			printf 'DTSTART;TZID=%s:%sT%s00\r\nDTEND;TZID=%s:%sT%s00\r\n' "$TZID" "$start" "$3" "$TZID" "$start" "$4"
		fi
		printf 'SUMMARY:%s\r\n' "$5"
		[ -z "$6" ] || printf 'LOCATION:%s\r\n' "$6"
		[ -z "$7" ] || printf 'DESCRIPTION:%s\r\n' "$7"
		[ -z "${8:-}" ] || printf 'RRULE:%s\r\n' "$8"
		printf 'END:VEVENT\r\nEND:VCALENDAR\r\n'
	} | dav -o /dev/null -w "%{http_code} event $1\n" -X PUT -H 'Content-Type: text/calendar; charset=utf-8' \
		--data-binary @- "$BASE$calendar$1.ics"
}

put_event planning 1 0930 1000 'Weekly planning' '' 'What ships this week.' 'FREQ=WEEKLY;BYDAY=MO'
put_event standup-tue 2 0915 0930 'Stand-up' '' ''
put_event lunch 2 1230 1330 'Lunch with Anna and Bert' 'The place on the corner' 'Their new menu.'
put_event q3 3 1400 1500 'Q3 report review' 'Meeting room 2' 'With Renée: the numbers in the attachment.'
put_event cluster 4 1000 1130 'Cluster test: kill the leader' '' 'Three nodes, one goes down, nothing lost.'
put_event dentist 4 1600 1645 'Dentist' 'Prinsengracht 1' ''
put_event release 5 allday '' 'Release day' '' ''
put_event demo 5 1500 1600 'robinmail demo' 'Online' 'Mail, calendar and contacts in one binary.'
put_event run 6 0900 1000 'Running club' 'Vondelpark' '' 'FREQ=WEEKLY;BYDAY=SA'

put_card() { # uid full-name given family email phone org title
	{
		printf 'BEGIN:VCARD\r\nVERSION:4.0\r\nUID:urn:uuid:%s\r\nFN:%s\r\nN:%s;%s;;;\r\n' "$1" "$2" "$4" "$3"
		printf 'EMAIL;TYPE=work:%s\r\n' "$5"
		[ -z "$6" ] || printf 'TEL;TYPE=cell:%s\r\n' "$6"
		[ -z "$7" ] || printf 'ORG:%s\r\n' "$7"
		[ -z "$8" ] || printf 'TITLE:%s\r\n' "$8"
		printf 'END:VCARD\r\n'
	} | dav -o /dev/null -w "%{http_code} contact $2\n" -X PUT -H 'Content-Type: text/vcard; charset=utf-8' \
		--data-binary @- "$BASE$book$1.vcf"
}

put_card 0a1d6c1e-0000-4000-8000-000000000001 'Anna de Vries' Anna 'de Vries' anna@sender.example '+31 6 00000001' 'Sender & Co' 'Designer'
put_card 0a1d6c1e-0000-4000-8000-000000000002 'Bert Jansen' Bert Jansen bert@sender.example '+31 6 00000002' 'Sender & Co' 'Developer'
put_card 0a1d6c1e-0000-4000-8000-000000000003 'Renée Dubois' Renée Dubois renee@partner.example '+33 6 00000003' 'Partner SA' 'Finance'
put_card 0a1d6c1e-0000-4000-8000-000000000004 'Sanne Bakker' Sanne Bakker sanne@studio.example '+31 6 00000004' 'Studio Bakker' 'Owner'
put_card 0a1d6c1e-0000-4000-8000-000000000005 'Tom Visser' Tom Visser tom@visser.example '' '' ''
put_card 0a1d6c1e-0000-4000-8000-000000000006 'Lotte de Boer' Lotte 'de Boer' lotte@boer.example '+31 6 00000006' '' 'Running club'
put_card 0a1d6c1e-0000-4000-8000-000000000007 'Daan Smit' Daan Smit daan@smit.example '+31 6 00000007' 'Smit IT' 'Sysadmin'
put_card 0a1d6c1e-0000-4000-8000-000000000008 'Weekly Digest' '' 'Weekly Digest' digest@news.example '' '' ''
