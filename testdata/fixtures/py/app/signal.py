class Signal:
    """The single repo-wide `send`; a local callback must not resolve here."""

    def send(self, payload: str) -> None:
        self.payload = payload
