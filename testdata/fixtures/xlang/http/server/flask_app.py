from flask import Flask

app = Flask(__name__)


@app.route("/api/items", methods=["POST"])
def add_item():
    return {}
