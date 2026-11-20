def dispatch(send) -> None:
    # `send` is a callback parameter, not the unique Signal.send method. The
    # repo-wide name guess is a false positive and must stay below the 0.5 floor.
    send("payload")
