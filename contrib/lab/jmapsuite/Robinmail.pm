package JMAP::TestSuite::ServerAdapter::Robinmail;
# The adapter of Fastmail's JMAP-TestSuite for robinmail, for the lab of
# contrib/lab. Every account a test asks for is a new user, made with
# "robinmail user add" (the image has the binary; the lab mounts the
# config and the master key). robinmail has one account per user and no
# sharing, so there is no pool_account_pair: the tests that copy between
# accounts are skipped.
use Moose;
with 'JMAP::TestSuite::ServerAdapter';

use IPC::Open3;
use Symbol qw(gensym);

our $STARTTIME = time();
our $USERNUM   = 1;

has base_uri    => (is => 'ro', required => 1);
has config      => (is => 'ro', required => 1);
has test_domain => (is => 'ro', required => 1);

sub any_account      { $_[0]->_new_account }
sub pristine_account { $_[0]->_new_account }

sub _new_account {
  my ($self) = @_;

  my $name = "jt-$STARTTIME-$$-" . $USERNUM++ . '@' . $self->test_domain;
  my $pass = join('-', map { sprintf('%08x', int(rand(0xFFFFFFFF))) } 1..4);

  my $pid = open3(my $in, my $out, my $err = gensym,
    'robinmail', '-config', $self->config, 'user', 'add', $name);
  print $in "$pass\n";
  close $in;
  my $said = do { local $/; <$out> } // '';
  my $complaint = do { local $/; <$err> } // '';
  waitpid $pid, 0;
  die "robinmail user add $name failed: $said$complaint\n" if $?;

  # "created jt-...@robin.test (account 5sj73e3vp7aa5nxl)"
  my ($account_id) = $said =~ /\(account ([^)]+)\)/
    or die "robinmail user add $name said: $said\n";

  return JMAP::TestSuite::Account::Robinmail->new({
    server    => $self,
    accountId => $account_id,
    username  => $name,
    password  => $pass,
  });
}

package JMAP::TestSuite::Account::Robinmail {
  use Moose;
  with 'JMAP::TestSuite::Account';

  use MIME::Base64 qw(encode_base64);
  use LWP::UserAgent;
  use JSON qw(decode_json);
  use JMAP::TestSuite::JMAP::Tester::WithSugar;

  has username => (is => 'ro', required => 1);
  has password => (is => 'ro', required => 1);

  sub authenticated_tester {
    my ($self) = @_;

    my $base  = $self->server->base_uri =~ s{/\z}{}r;
    my $creds = encode_base64($self->username . ':' . $self->password, '');

    my $lwp = LWP::UserAgent->new;
    $lwp->default_header(Authorization => "Basic $creds");
    my $res = $lwp->get("$base/jmap/session");
    die "session of " . $self->username . ": " . $res->status_line . "\n"
      unless $res->is_success;
    my $session = decode_json($res->decoded_content);

    # What the server says it has, so tests for what it lacks skip.
    my @using = sort keys %{ $session->{capabilities} // {} };

    my $tester = JMAP::TestSuite::JMAP::Tester::WithSugar->new({
      api_uri            => "$base/jmap/api",
      authentication_uri => "$base/jmap/session",
      upload_uri         => "$base/jmap/upload/{accountId}/",
      download_uri       => "$base/jmap/download/{accountId}/{blobId}/{name}",
    });
    $tester->ua->set_default_header(Authorization => "Basic $creds");
    $tester->default_using(\@using);

    return $tester;
  }

  no Moose;
  __PACKAGE__->meta->make_immutable;
}

no Moose;
__PACKAGE__->meta->make_immutable;
