# upgradescope README demo (docs/img/demo.gif). Render from the repo root:
#   make build && vhs hack/demo/demo.tape
# Needs vhs (with ttyd and ffmpeg) and bin/upgradescope. No cluster: it
# scans the rendered manifests in hack/demo/rendered, so anyone can
# re-record it and get the same frames for the same binary.

Output docs/img/demo.gif

Set Shell bash
Set FontSize 14
Set Width 1100
Set Height 760
Set Padding 16
Set Theme "Dracula"
Set Framerate 20
Set TypingSpeed 35ms

# Put the binary on PATH under its own name, work from the demo directory
# so paths print short, and start from a clean screen.
Hide
Type `cd hack/demo && export PATH="$PWD/../../bin:$PATH" PS1='$ ' && clear`
Enter
Sleep 500ms
Show

Type "upgradescope scan --files rendered/ --target 1.37"
Sleep 400ms
Enter
Sleep 6s

Type "echo $?"
Sleep 300ms
Enter
Sleep 1500ms

Type "upgradescope scan --files rendered/ --target 1.37 --output json | jq -c '{verdict, score}'"
Sleep 400ms
Enter
Sleep 3s
