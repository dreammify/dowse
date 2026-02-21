# typed: strict
# frozen_string_literal: true

module Core
  module StringUtils
    extend T::Sig

    sig { params(text: String).returns(String) }
    def self.capitalize_words(text)
      text.split(" ").map(&:capitalize).join(" ")
    end

    sig { params(text: String, max_length: Integer, ellipsis: String).returns(String) }
    def self.truncate(text, max_length, ellipsis = "...")
      if text.length <= max_length
        text
      else
        T.must(text[0, max_length - ellipsis.length]) + ellipsis
      end
    end
  end
end
