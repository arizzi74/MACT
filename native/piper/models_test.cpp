#include "models.h"
#include <string>

int main() {
    const auto italian = piper::model_for("italian");
    const auto english = piper::model_for("english");
    if (std::string(italian.directory) != "vits-piper-it_IT-miro-high" || std::string(italian.filename) != "it_IT-miro-high.onnx") return 1;
    if (std::string(english.directory) != "vits-piper-en_US-ryan-medium" || std::string(english.filename) != "en_US-ryan-medium.onnx") return 2;
    for (const auto &language : {"", "french", "../italian", "/english"}) {
        try { piper::model_for(language); return 3; }
        catch (const std::runtime_error &) {}
    }
    return 0;
}
