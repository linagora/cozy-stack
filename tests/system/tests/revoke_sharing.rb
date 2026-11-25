require_relative '../boot'
require 'minitest/autorun'
require 'pry-rescue/minitest' unless ENV['CI']

def assert_eventually(timeout: 10, interval: 0.1)
  deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
  begin
    yield
  rescue Minitest::Assertion
    raise if Process.clock_gettime(Process::CLOCK_MONOTONIC) >= deadline
    sleep interval
    retry
  end
end

def assert_not_found(domain, doctype, id)
  assert_raises RestClient::NotFound do
    Helpers.couch.get_doc domain, doctype, id
  end
end

def assert_oauth_client_not_empty(client_id)
  refute_empty client_id
end

def assert_triggers_not_empty(triggers_ids)
  triggers_ids.each { |id| refute_empty id }
end

def assert_no_triggers(inst, triggers_ids)
  triggers_ids.each { |id| assert_not_found inst.domain, "io.cozy.triggers", id }
end

def assert_no_oauth_client(inst, client_id)
  assert_not_found inst.domain, "io.cozy.oauth", client_id
end

def triggers_ids(sharing)
  track_id = sharing.dig "triggers", "track_ids", 0
  replicate_id = sharing.dig "triggers", "replicate_id"
  upload_id = sharing.dig "triggers", "upload_id"
  [track_id, replicate_id, upload_id]
end

def inbound_client_id(sharing, index)
  sharing.dig "credentials", index, "inbound_client_id"
end

def assert_sharing_revoked(inst, sharing_id, is_sharer)
  doc = Helpers.couch.get_doc inst.domain, Sharing.doctype, sharing_id
  assert_nil doc["active"]
  assert_empty doc["triggers"]
  if is_sharer
    doc["credentials"].each { |cred| assert_empty cred }
    doc["members"].each_with_index do |m, i|
      assert_equal "revoked", m["status"] if i != 0
    end
  else
    assert_nil doc["credentials"]
  end
  shared_docs = Sharing.get_shared_docs(inst, sharing_id, Folder.doctype)
  assert_nil shared_docs
end

def assert_recipient_revoked(inst, sharing_id, index)
  doc = Helpers.couch.get_doc inst.domain, Sharing.doctype, sharing_id
  assert doc["active"]
  refute_empty doc["triggers"]
  assert_empty doc.dig("credentials", index - 1)
  assert_equal("revoked", doc.dig("members", index, "status"))
  shared_docs = Sharing.get_shared_docs(inst, sharing_id, Folder.doctype)
  refute_empty shared_docs
end

describe "A sharing" do
  it "can be revoked" do
    Helpers.scenario "revoke_sharing"
    Helpers.start_mailhog

    bob = "Bob"
    charlie = "Charlie"
    dave = "Dave"
    emily = "Emily"
    lastname = Faker::Name.last_name

    # Create the instances
    inst_alice = Instance.create name: "Alice"
    inst_bob = Instance.create name: bob
    inst_charlie = Instance.create name: charlie
    inst_dave = Instance.create name: dave
    inst_emily = Instance.create name: emily

    # Create the contacts
    contact_bob = Contact.create inst_alice, given_name: bob
    contact_charlie = Contact.create inst_alice, given_name: charlie
    Contact.create inst_bob, given_name: "Alice",
                             family_name: lastname,
                             email: "alice+test@localhost"
    Contact.create inst_bob, given_name: "Charlie",
                             family_name: lastname,
                             email: contact_charlie.primary_email
    contact_dave = Contact.create inst_bob, given_name: dave,
                                            family_name: lastname
    contact_emily = Contact.create inst_bob, given_name: emily,
                                             family_name: lastname

    # Create the folder
    folder = Folder.create inst_alice
    folder.couch_id.wont_be_empty
    file = "../fixtures/wet-cozy_20160910__M4Dz.jpg"
    opts = CozyFile.options_from_fixture(file, dir_id: folder.couch_id)
    file = CozyFile.create inst_alice, opts

    # Create the sharing
    sharing = Sharing.new
    sharing.rules << Rule.sync(folder)
    sharing.members << inst_alice << contact_bob
    inst_alice.register sharing

    # Check members status
    doc = Helpers.couch.get_doc inst_alice.domain, Sharing.doctype, sharing.couch_id
    owner = doc["members"].first
    assert_equal "owner", owner["status"]
    assert_equal "Alice", owner["public_name"]
    assert_equal "alice+test@localhost", owner["email"]
    assert_equal inst_alice.url, owner["instance"]
    recpt1 = doc["members"][1]
    assert_equal "pending", recpt1["status"]
    assert_equal contact_bob.primary_email, recpt1["email"]

    # Accept the sharing
    sleep 1
    inst_bob.accept sharing
    sleep 2

    # Get the clients id and triggers id
    doc = Helpers.couch.get_doc inst_alice.domain, Sharing.doctype, sharing.couch_id
    client_id = inbound_client_id doc, 0
    tri_ids = triggers_ids doc
    assert_oauth_client_not_empty client_id
    assert_triggers_not_empty tri_ids

    doc = Helpers.couch.get_doc inst_bob.domain, Sharing.doctype, sharing.couch_id
    client_id_recipient = inbound_client_id doc, 0
    tri_ids_recipient = triggers_ids doc
    assert_oauth_client_not_empty client_id_recipient
    assert_triggers_not_empty tri_ids_recipient

    # The instance URL has been added to the contact document
    contact = Contact.find inst_alice, contact_bob.couch_id
    assert_equal inst_bob.url, contact.cozy[0]["url"]

    # Revoke the sharing
    code = sharing.revoke_by_sharer(inst_alice, Folder.doctype)
    assert_equal 204, code

    # Check the sharing on the sharer
    assert_sharing_revoked inst_alice, sharing.couch_id, true
    assert_no_triggers inst_alice, tri_ids
    assert_no_oauth_client inst_alice, client_id

    # Recipient cleanup runs in a background job.
    assert_eventually do
      assert_sharing_revoked inst_bob, sharing.couch_id, false
      assert_no_oauth_client inst_bob, client_id_recipient
      assert_no_triggers inst_bob, tri_ids_recipient
    end

    # Make an update: it should not be propagated
    old_name = file.name
    file.rename inst_alice, Faker::Internet.slug
    sleep 3
    # XXX: the folder name was changed after the sharing was revoked
    folder_path = "/#{Helpers::SHARED_WITH_ME}/#{folder.name} #{Folder::CANCELLED_SUFFIX}"
    file_path = "#{folder_path}/#{old_name}"
    file_recipient = Folder.find_by_path inst_bob, file_path
    refute_equal file_recipient.name, file.name

    # Create a new sharing with a new folder
    folder = Folder.create inst_alice
    file = "../fixtures/wet-cozy_20160910__M4Dz.jpg"
    opts = CozyFile.options_from_fixture(file, dir_id: folder.couch_id)
    file = CozyFile.create inst_alice, opts
    sharing = Sharing.new
    sharing.rules << Rule.sync(folder)
    sharing.members << inst_alice << contact_bob
    inst_alice.register sharing

    # Accept the sharing
    sleep 1
    inst_bob.accept sharing
    sleep 3

    # Add Charlie, Dave, and Emily to the sharing
    code = sharing.add_members inst_alice, [contact_charlie]
    assert_equal 200, code
    sleep 1
    inst_charlie.accept sharing
    sleep 6
    code = sharing.add_members inst_bob, [contact_dave]
    assert_equal 200, code
    code = sharing.add_members inst_bob, [contact_emily]
    assert_equal 200, code
    sleep 1
    inst_dave.accept sharing, inst_bob
    inst_emily.accept sharing, inst_bob
    sleep 6

    # Get the clients id and triggers id on alice side
    doc = Helpers.couch.get_doc inst_alice.domain, Sharing.doctype, sharing.couch_id
    inb_bob_client_id = inbound_client_id doc, 0
    inb_charlie_client_id = inbound_client_id doc, 1
    tri_ids = triggers_ids doc
    assert_oauth_client_not_empty inb_bob_client_id
    assert_oauth_client_not_empty inb_charlie_client_id
    assert_triggers_not_empty tri_ids

    # Get the clients id and triggers id on bob side
    doc = Helpers.couch.get_doc inst_bob.domain, Sharing.doctype, sharing.couch_id
    client_id_bob = inbound_client_id doc, 0
    tri_ids_bob = triggers_ids doc
    assert_oauth_client_not_empty client_id_bob
    assert_triggers_not_empty tri_ids_bob

    # The instance URL has been added to the contact document
    contact = Contact.find inst_bob, contact_dave.couch_id
    assert_equal inst_dave.url, contact.cozy[0]["url"]

    # Check that Bob has all info about the members of this sharing
    owner = doc["members"].first
    assert_equal "owner", owner["status"]
    assert_equal "Alice", owner["public_name"]
    assert_equal "Alice #{lastname}", owner["name"]
    assert_equal "alice+test@localhost", owner["email"]
    assert_equal inst_alice.url, owner["instance"]
    recpt1 = doc["members"][1]
    assert_equal "ready", recpt1["status"]
    assert_equal "Bob", recpt1["public_name"]
    assert_equal contact_bob.primary_email, recpt1["email"]
    assert_equal inst_bob.url, recpt1["instance"]
    assert_nil recpt1["name"]
    recpt2 = doc["members"][2]
    assert_equal "ready", recpt2["status"]
    assert_equal "Charlie", recpt2["public_name"]
    assert_equal contact_charlie.primary_email, recpt2["email"]
    assert_equal "Charlie #{lastname}", recpt2["name"]
    assert_nil recpt2["instance"]
    recpt3 = doc["members"][3]
    assert_equal "ready", recpt3["status"]
    assert_equal "Dave", recpt3["public_name"]
    assert_equal contact_dave.primary_email, recpt3["email"]
    assert_equal "Dave #{lastname}", recpt3["name"]
    assert_nil recpt3["instance"]
    recpt4 = doc["members"][4]
    assert_equal "ready", recpt4["status"]
    assert_equal "Emily", recpt4["public_name"]
    assert_equal contact_emily.primary_email, recpt4["email"]
    assert_equal "Emily #{lastname}", recpt4["name"]
    assert_nil recpt4["instance"]

    # Get the clients id and triggers id on charlie side
    doc = Helpers.couch.get_doc inst_charlie.domain, Sharing.doctype, sharing.couch_id
    client_id_charlie = inbound_client_id doc, 0
    tri_ids_charlie = triggers_ids doc
    assert_oauth_client_not_empty client_id_charlie
    assert_triggers_not_empty tri_ids_charlie

    # Revoke bob by himself
    code = sharing.revoke_recipient_by_itself inst_bob, Folder.doctype
    assert_equal 204, code

    # Check the sharing on alice
    assert_recipient_revoked inst_alice, sharing.couch_id, 1
    assert_no_oauth_client inst_alice, inb_bob_client_id
    assert_oauth_client_not_empty inb_charlie_client_id
    assert_triggers_not_empty tri_ids

    # Check the sharing on bob
    assert_sharing_revoked inst_bob, sharing.couch_id, false
    assert_no_triggers inst_bob, tri_ids_bob
    assert_no_oauth_client inst_bob, client_id_bob

    # Check that Charlie has all info about the members of this sharing
    sleep 5
    doc = Helpers.couch.get_doc inst_charlie.domain, Sharing.doctype, sharing.couch_id
    owner = doc["members"].first
    assert_equal "owner", owner["status"]
    assert_equal "Alice", owner["public_name"]
    assert_equal "alice+test@localhost", owner["email"]
    assert_equal inst_alice.url, owner["instance"]
    assert_nil owner["name"]
    recpt1 = doc["members"][1]
    assert_equal "revoked", recpt1["status"]
    assert_equal "Bob", recpt1["public_name"]
    assert_equal contact_bob.primary_email, recpt1["email"]
    assert_nil recpt1["name"]
    assert_nil recpt1["instance"]
    recpt2 = doc["members"][2]
    assert_equal "ready", recpt2["status"]
    assert_equal "Charlie", recpt2["public_name"]
    assert_equal contact_charlie.primary_email, recpt2["email"]
    assert_equal inst_charlie.url, recpt2["instance"]
    assert_nil recpt2["name"]
    recpt3 = doc["members"][3]
    assert_equal "ready", recpt3["status"]
    assert_equal "Dave", recpt3["public_name"]
    assert_equal contact_dave.primary_email, recpt3["email"]
    assert_nil recpt3["instance"]
    recpt4 = doc["members"][4]
    assert_equal "ready", recpt4["status"]
    assert_equal "Emily", recpt4["public_name"]
    assert_equal contact_emily.primary_email, recpt4["email"]
    assert_nil recpt4["instance"]

    # Revoke charlie and dave by alice
    code = sharing.revoke_recipient_by_sharer inst_alice, Folder.doctype, 2
    assert_equal 204, code
    code = sharing.revoke_recipient_by_sharer inst_alice, Folder.doctype, 3
    assert_equal 204, code

    # Emily puts the shared folder in the trashed
    folder_path = "/#{Helpers::SHARED_WITH_ME}/#{folder.name}"
    folder_emily = Folder.find_by_path inst_emily, folder_path
    folder_emily.remove inst_emily
    sleep 1

    # Check the sharing on alice
    assert_sharing_revoked inst_alice, sharing.couch_id, true
    assert_no_oauth_client inst_alice, inb_bob_client_id
    assert_no_oauth_client inst_alice, inb_charlie_client_id
    assert_no_triggers inst_alice, tri_ids

    # Check the sharing on charlie
    assert_sharing_revoked inst_charlie, sharing.couch_id, false
    assert_no_triggers inst_charlie, tri_ids_charlie
    assert_no_oauth_client inst_charlie, client_id_charlie

    # Check the sharing on emily
    assert_sharing_revoked inst_emily, sharing.couch_id, false

    # Make an update: it should not be propagated
    old_name = file.name
    file.rename inst_alice, Faker::Internet.slug
    sleep 3
    # XXX: the folder name was changed after the sharing was revoked
    folder_path = "/#{Helpers::SHARED_WITH_ME}/#{folder.name} #{Folder::CANCELLED_SUFFIX}"
    file_path = "#{folder_path}/#{old_name}"
    file_bob = Folder.find_by_path inst_bob, file_path
    file_charlie = Folder.find_by_path inst_charlie, file_path
    refute_equal file_bob.name, file.name
    refute_equal file_charlie.name, file.name

    instances = [inst_alice, inst_bob, inst_charlie, inst_dave]
    instances.each do |inst|
      Folder.clear_trash inst
      assert_equal inst.check, []
    end
    instances.each(&:remove)
  end
end
