# Records docs/demo.gif. Run from the repo root: vhs docs/demo.tape
# Needs Go, curl, jq and sqlite3. Everything shown is made up; the "provider" is examples/fake-provider.

Output docs/demo.gif

Set Shell "bash"
Set FontSize 16
Set Width 1320
Set Height 500
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 28ms
Set Framerate 20
Env PS1 "$ "
Env BASH_SILENCE_DEPRECATION_WARNING "1"

Hide
Type "source docs/demo/setup.sh && clear"
Enter
Sleep 15s
Show

Type "# 1. Dry run: what would veilox mask in this ticket?"
Enter
Type "veilox scan ticket.txt"
Enter
Sleep 6s

Type "clear"
Enter
Type "# 2. Your app calls veilox instead of Anthropic: same API, its own key"
Enter
Type `curl -s localhost:8080/v1/messages -H "x-api-key: $VEILOX_KEY" -d @request.json | jq -r '.content[0].text' | fold -s`
Enter
Sleep 3s
Type "# ...and this is all the provider saw:"
Enter
Type "cat provider.log"
Enter
Sleep 6s

Type "clear"
Enter
Type "# 3. What did AI cost this week, per app?"
Enter
Type "veilox report --since 7d --by app,model"
Enter
Sleep 7s

Hide
Type "demo_stop"
Enter
