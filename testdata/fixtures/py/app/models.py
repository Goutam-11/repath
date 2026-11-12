class Base:
    def identify(self) -> str:
        return "base"


class User(Base):
    def __init__(self, name: str):
        self.name = name

    def label(self) -> str:
        return self.greet()

    def greet(self) -> str:
        return "hi " + self.name
