from models import Base


class Cart(Base):
    pass


def make() -> Cart:
    return Cart()
