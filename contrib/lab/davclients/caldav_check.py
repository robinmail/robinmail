"""What the python "caldav" library does against robinmail, step by step.

    python3 /lab/caldav_check.py URL USER PASSWORD TOKEN

Every step prints one line "ok <step> <detail>"; the first that fails
raises, and the exit status is not 0. TOKEN goes into the names, so the
test finds what this made.
"""
import datetime
import sys

import caldav

url, user, password, token = sys.argv[1:5]
client = caldav.DAVClient(url, username=user, password=password, ssl_verify_cert="/lab/secrets/ca.crt")


def ok(step, detail=""):
    print("ok", step, detail, flush=True)


principal = client.principal()
ok("principal", principal.url)
cals = principal.calendars()
assert cals, "no calendars"
ok("calendars", len(cals))

cal = principal.make_calendar(name="lab " + token)
ok("make_calendar", cal.url)

start = datetime.datetime(2031, 6, 2, 9, 0, tzinfo=datetime.timezone.utc)
ev = cal.save_event(dtstart=start, dtend=start + datetime.timedelta(hours=1), summary="lab event " + token,
                    rrule={"FREQ": "DAILY", "COUNT": 3})
ok("save_event", ev.url)

found = cal.search(start=start, end=start + datetime.timedelta(days=7), event=True, expand=True)
assert len(found) == 3, "expanded %d instances, want 3" % len(found)
ok("search_expand", len(found))

found = cal.search(start=start + datetime.timedelta(days=30), end=start + datetime.timedelta(days=31), event=True)
assert len(found) == 0, "an event in an empty range"
ok("search_empty", 0)

ev.load()
ev.vobject_instance.vevent.summary.value = "lab event changed " + token
ev.save()
again = cal.event_by_url(ev.url)
assert "changed" in again.data, again.data
ok("update", "")

objs = cal.objects_by_sync_token(load_objects=True)
token1 = objs.sync_token
assert len(list(objs)) == 1, "sync: %d objects" % len(list(objs))
second = cal.save_event(dtstart=start + datetime.timedelta(days=10), dtend=start + datetime.timedelta(days=10, hours=1),
                        summary="lab second " + token)
changed = cal.objects_by_sync_token(sync_token=token1, load_objects=True)
assert len(list(changed)) == 1, "sync after one change: %d" % len(list(changed))
ok("sync_token", changed.sync_token)

fb = cal.freebusy_request(start, start + datetime.timedelta(days=1))
assert "FREEBUSY" in fb.data, fb.data
ok("freebusy", "")

second.delete()
gone = cal.objects_by_sync_token(sync_token=changed.sync_token, load_objects=False)
ok("delete", len(list(gone)))
