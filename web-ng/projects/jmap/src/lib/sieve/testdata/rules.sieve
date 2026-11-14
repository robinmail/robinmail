# robinmail rules 1
# Written by the Filters page of the webmail. The page reads it back as
# rules; a script changed by hand is shown there as a script only.
require ["fileinto", "mailbox", "copy", "imap4flags"];

# rule: Invoices
if allof (address :contains "from" "billing@", header :contains "subject" "invoice") {
    addflag "\\Seen";
    fileinto :create "Work/Invoices";
    stop;
}

# rule: Lists
if anyof (address :contains ["to", "cc"] "dev@lists.example", header :contains "subject" "[dev]") {
    fileinto :copy :create "Lists";
    redirect :copy "archive@example.org";
}

# rule: No "offers"
if header :contains "subject" "special \\ \"offer\"" {
    discard;
    stop;
}
