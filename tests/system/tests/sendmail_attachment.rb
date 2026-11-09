require_relative '../boot'
require 'minitest/autorun'
require 'pry-rescue/minitest' unless ENV['CI']

describe "Sending a mail" do
  it "can be done with an attachment" do
    Helpers.scenario "sendmail_attachment"
    Helpers.start_mailhog

    inst = Instance.create locale: "en"
    bytes = File.read("../fixtures/wet-cozy_20160910__M4Dz.jpg")
    bytes = bytes.force_encoding(Encoding::BINARY)
    encoded = Base64.encode64(bytes)
    args = {
      mode: "from",
      to: [
        { name: "Jane Doe", email: "jane@cozy.tools" }
      ],
      subject: "attachment test",
      parts: [
        { type: "text/html", body: "<html><body><p>This is a test!</p></body></html>" },
        { type: "text/plain", body: "This is a test!" }
      ],
      attachments: [
        { filename: "wet.jpg", content: encoded }
      ]
    }
    Job.create inst, "sendmail", args

    received = []
    30.times do
      sleep 1
      received = Email.received kind: "to", query: "jane@cozy.tools"
      break if received.any?
    end
    refute_empty received
    mail = received.first
    assert_equal mail.subject, args[:subject]
    assert mail.parts.length > 2
    disposition = mail.parts.dig 1, "Headers", "Content-Disposition", 0
    assert_equal "attachment; filename=wet.jpg", disposition
    encoding = mail.parts.dig 1, "Headers", "Content-Transfer-Encoding", 0
    assert_equal encoding, "base64"
    ctype = mail.parts.dig(1, "Headers", "Content-Type", 0).split(";")[0]
    assert_equal ctype, "image/jpeg"
    decoded = Base64.decode64(mail.parts.dig(1, "Body"))
    assert_equal decoded, bytes

    inst.remove
  end
end
